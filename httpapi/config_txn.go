package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// Default transaction TTL when the client does not specify one.
const defaultTxnTTL = 5 * time.Minute

// Maximum allowed transaction TTL.
const maxTxnTTL = 30 * time.Minute

// commitApplyTimeout bounds the in-band apply of a committed config. The apply
// (build/connect/drain) runs on a context DETACHED from the request so a client
// disconnect cannot cancel the runtime swap after the durable write; this
// timeout is the sole cap on that detached work. It is deliberately generous
// (an apply can legitimately take tens of seconds) — if it is exceeded the
// commit treats it as an apply failure: the previous on-disk config is restored
// and the commit reports rolled_back (the file watcher re-emit is the safety
// net that eventually converges the runtime).
const commitApplyTimeout = 60 * time.Second

// Sentinel errors returned by configTxnManager methods.
var (
	errTxnActive       = errors.New("another transaction is already active")
	errTxnNotFound     = errors.New("transaction not found")
	errTxnExpired      = errors.New("transaction has expired")
	errVersionConflict = errors.New("config version conflict")
	// errConfigOptionsLoss signals that a commit was refused because it would
	// erase an existing entry's typed plugin options (the CRITICAL corruption
	// class). It is a client-correctable condition (fix the patch), mapped to
	// 422 by the handler.
	errConfigOptionsLoss = errors.New("config commit would erase plugin options")
	// errConfigApplyFailed signals that a commit durably wrote the new config
	// to disk but the in-band apply to the running runtime failed AND the
	// previous on-disk config could NOT be restored (first write, or the
	// restore write itself failed). Disk and runtime have diverged and the
	// operator must reconcile by hand. Distinct so the handler can report
	// "committed to disk but not applied".
	errConfigApplyFailed = errors.New("config committed but apply failed")
	// errConfigRolledBack signals that a commit durably wrote the new config
	// but the in-band apply failed and the PREVIOUS on-disk config was
	// successfully restored, so a process restart recovers the last good config
	// instead of crash-looping on the rejected one. Distinct from
	// errConfigApplyFailed so the handler reports rolled_back rather than
	// committed_not_applied.
	errConfigRolledBack = errors.New("config apply failed; on-disk config rolled back to previous version")
	// errConfigStoreNotCAS signals that a durable commit was REFUSED because the
	// configured ConfigStore does not implement ports.ConditionalConfigStore
	// (compare-and-swap) AND the operator has not asserted single-writer
	// (Config.ConfigSingleWriter). A plain last-writer-wins Save on a shared
	// non-CAS backend can silently clobber a peer admin instance's acknowledged
	// commit, so the durable write is refused rather than performed silently
	// It is a deployment-configuration condition, not a
	// client-correctable one: either wire a CAS store or assert single-writer.
	errConfigStoreNotCAS = errors.New("config commit refused: non-CAS store is cluster-unsafe")
)

// ConfigTransaction represents an in-progress configuration change.
type ConfigTransaction struct {
	ID         string    `json:"txn_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	PatchCount int       `json:"patch_count"`

	baseVersion int // config version when the transaction was created
	patches     []*ports.BridgeConfig
	merged      *ports.BridgeConfig
}

// configTxnManager manages the single active config transaction.
// All methods are safe for concurrent use.
type configTxnManager struct {
	mu sync.Mutex
	// commitMu serializes the whole of Commit — the durable write, the
	// out-of-lock apply, and any rollback — so only one commit is ever in that
	// pipeline at a time. commitDurable clears m.active and releases m.mu BEFORE
	// the (slow) apply so Preview/status stay responsive; without commitMu a
	// second Begin+Commit could advance the on-disk version during that apply
	// window, and a failed apply's rollback would then clobber the newer version
	// back to this txn's prior (silent loss of an acknowledged commit + version
	// regression). commitMu is the coarser gate: it blocks only other commits,
	// never the read/preview endpoints.
	commitMu       sync.Mutex
	active         *ConfigTransaction
	store          ports.ConfigStore
	configProvider func() *ports.BridgeConfig
	applier        func(ctx context.Context, cfg *ports.BridgeConfig) error
	logger         *slog.Logger
	clk            clock.Clock
	timeoutTimer   clock.Timer
	timeoutCancel  chan struct{}
	// singleWriter records the operator's assertion that THIS process is the
	// sole writer of the durable config store. It gates the non-CAS durable
	// commit path in commitDurable: a store that does not implement
	// ports.ConditionalConfigStore can be committed to with a plain (last-
	// writer-wins) Save ONLY when there is a single writer that cannot clobber a
	// peer's acknowledged commit; otherwise the commit is refused
	// (errConfigStoreNotCAS) rather than silently doing LWW. newTxnManager
	// defaults it to true for the direct, in-process, single-manager
	// construction used by tests and embedders; Server.New overrides it from
	// Config.ConfigSingleWriter so a real deployment fails closed on a shared
	// non-CAS store by default.
	singleWriter bool
}

// newTxnManager creates a new transaction manager.
// store is the persistence boundary used for validate/merge/save/load.
// provider returns the current effective config (e.g. Supervisor.Config).
// applier, when non-nil, is invoked after a successful Commit Save to apply
// the new config to the running runtime in-band; nil delegates application to
// the config watcher.
func newTxnManager(store ports.ConfigStore, provider func() *ports.BridgeConfig, applier func(context.Context, *ports.BridgeConfig) error, logger *slog.Logger, clk clock.Clock) *configTxnManager {
	if clk == nil {
		clk = clock.System
	}
	return &configTxnManager{
		store:          store,
		configProvider: provider,
		applier:        applier,
		logger:         logger,
		clk:            clk,
		// Direct construction is single-manager (one in-process writer), so it
		// is single-writer by definition. Server.New overrides this from
		// Config.ConfigSingleWriter to fail closed on a shared non-CAS store in
		// a real (possibly multi-instance) deployment.
		singleWriter: true,
	}
}

// Begin starts a new config transaction. Returns errTxnActive if a
// transaction is already in progress. The ttl controls how long the
// transaction remains active before auto-rollback; zero uses the default.
func (m *configTxnManager) Begin(ctx context.Context, ttl time.Duration) (*ConfigTransaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active != nil {
		return nil, errTxnActive
	}

	if ttl <= 0 {
		ttl = defaultTxnTTL
	}
	if ttl > maxTxnTTL {
		ttl = maxTxnTTL
	}

	now := m.clk.Now().UTC()

	// Baseline the optimistic-concurrency version against the ON-DISK config,
	// not the in-memory running config. The running config can lag disk (an
	// operator hand-edited the file, or another instance committed) or lead
	// it; baselining off memory would let a stale in-memory version pass the
	// commit-time CAS and silently clobber the newer file. Reading disk here
	// makes Begin and Commit check-and-set against the same source of truth.
	baseVersion, err := m.readDiskVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("config txn begin: read disk version: %w", err)
	}

	txn := &ConfigTransaction{
		ID:          generateTxnID(),
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		baseVersion: baseVersion,
	}
	m.active = txn

	m.timeoutTimer = m.clk.NewTimer(ttl)
	m.timeoutCancel = make(chan struct{})
	timer := m.timeoutTimer
	cancel := m.timeoutCancel
	txnID := txn.ID
	go func() {
		select {
		case <-timer.C():
			m.expire(txnID)
		case <-cancel:
		}
	}()

	return txn, nil
}

// Patch applies a config overlay to the active transaction. It merges the
// overlay on top of the current effective config plus all previous patches,
// validates the result, and returns the merged preview along with any
// validation warnings. Returns an error if the merged config is invalid.
func (m *configTxnManager) Patch(ctx context.Context, txnID string, overlay *ports.BridgeConfig) (*ports.BridgeConfig, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, nil, err
	}

	m.active.patches = append(m.active.patches, overlay)
	m.active.PatchCount++
	m.active.merged = nil // invalidate cache

	merged, err := m.computeMerged(ctx)
	if err != nil {
		// Remove the bad patch so the transaction remains usable.
		m.active.patches = m.active.patches[:len(m.active.patches)-1]
		m.active.PatchCount--
		return nil, nil, err
	}

	warnings, valErr := m.store.Validate(ctx, merged)
	if valErr != nil {
		// Remove the bad patch.
		m.active.patches = m.active.patches[:len(m.active.patches)-1]
		m.active.PatchCount--
		return nil, warnings, valErr
	}

	m.active.merged = merged
	return merged, warnings, nil
}

// txnMeta is an immutable snapshot of a transaction's identity and timing,
// captured under the manager lock. Returning it alongside the preview lets a
// handler render the response from ONE consistent snapshot rather than a second
// Active() read that a concurrent TTL-expiry or DELETE can race to nil (which
// then panics on a field access) or, worse, to a DIFFERENT transaction.
type txnMeta struct {
	ID         string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	PatchCount int
}

// metaLocked snapshots the active transaction's identity/timing. Must be called
// with mu held and only after checkTxn has confirmed m.active is non-nil.
func (m *configTxnManager) metaLocked() txnMeta {
	return txnMeta{
		ID:         m.active.ID,
		CreatedAt:  m.active.CreatedAt,
		ExpiresAt:  m.active.ExpiresAt,
		PatchCount: m.active.PatchCount,
	}
}

// Preview returns the current merged state of the transaction without
// adding a new patch, together with a consistent metadata snapshot taken under
// the same lock (so the caller never re-reads a transaction that a concurrent
// expiry/rollback has cleared or replaced).
func (m *configTxnManager) Preview(ctx context.Context, txnID string) (*ports.BridgeConfig, txnMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, txnMeta{}, err
	}

	meta := m.metaLocked()

	if m.active.merged != nil {
		return m.active.merged, meta, nil
	}

	merged, err := m.computeMerged(ctx)
	if err != nil {
		return nil, txnMeta{}, err
	}
	m.active.merged = merged
	return merged, meta, nil
}

// guardNoConfigLoss returns an error when any plugin-config-bearing entry that
// carried a non-nil typed Config in base is present in merged with a nil
// Config. See Commit for the rationale.
func guardNoConfigLoss(base, merged *ports.BridgeConfig) error {
	if base == nil || merged == nil {
		return nil
	}

	mSessions := make(map[string]ports.PluginConfig, len(merged.Sessions))
	for i := range merged.Sessions {
		mSessions[merged.Sessions[i].ID] = merged.Sessions[i].Config
	}
	for i := range base.Sessions {
		if base.Sessions[i].Config == nil {
			continue
		}
		if cfg, ok := mSessions[base.Sessions[i].ID]; ok && cfg == nil {
			return configLossError("session", base.Sessions[i].ID)
		}
	}

	mReceivers := make(map[string]ports.PluginConfig, len(merged.Receivers))
	for i := range merged.Receivers {
		mReceivers[merged.Receivers[i].ID] = merged.Receivers[i].Config
	}
	for i := range base.Receivers {
		if base.Receivers[i].Config == nil {
			continue
		}
		if cfg, ok := mReceivers[base.Receivers[i].ID]; ok && cfg == nil {
			return configLossError("receiver", base.Receivers[i].ID)
		}
	}

	mSenders := make(map[string]ports.PluginConfig, len(merged.Senders))
	for i := range merged.Senders {
		mSenders[merged.Senders[i].ID] = merged.Senders[i].Config
	}
	for i := range base.Senders {
		if base.Senders[i].Config == nil {
			continue
		}
		if cfg, ok := mSenders[base.Senders[i].ID]; ok && cfg == nil {
			return configLossError("sender", base.Senders[i].ID)
		}
	}

	mBindings := make(map[string]ports.PluginConfig, len(merged.Bindings))
	for i := range merged.Bindings {
		mBindings[merged.Bindings[i].ID] = merged.Bindings[i].Config
	}
	for i := range base.Bindings {
		if base.Bindings[i].Config == nil {
			continue
		}
		if cfg, ok := mBindings[base.Bindings[i].ID]; ok && cfg == nil {
			return configLossError("binding", base.Bindings[i].ID)
		}
	}

	return nil
}

func configLossError(kind, id string) error {
	return fmt.Errorf("%w: %s %q would lose its plugin options (broker URL/credentials); "+
		"patch the entry with its full options block via a file edit, or omit it from the patch to keep the existing options",
		errConfigOptionsLoss, kind, id)
}

// readDiskVersion reads the current config from the underlying store
// and returns its version. Returns 0 (with no error) when the store
// reports fs.ErrNotExist or shared.ErrNotFound, including wrapped errors.
// The txn API signals first-write semantics via baseVersion=0.
func (m *configTxnManager) readDiskVersion(ctx context.Context) (int, error) {
	diskCfg, err := m.store.Load(ctx)
	if err != nil {
		if isConfigNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	return diskCfg.Version, nil
}

// Rollback discards the active transaction.
func (m *configTxnManager) Rollback(txnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return err
	}

	m.cleanup()
	return nil
}

// expire is called by the timeout timer to auto-rollback a stale transaction.
func (m *configTxnManager) expire(txnID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active == nil || m.active.ID != txnID {
		return
	}

	if m.logger != nil {
		m.logger.Warn("config transaction expired, rolling back",
			"txn_id", m.active.ID,
			"created_at", m.active.CreatedAt,
		)
	}
	m.cleanupLocked()
}

// checkTxn verifies the given txnID matches the active transaction.
// Must be called with mu held.
func (m *configTxnManager) checkTxn(txnID string) error {
	if m.active == nil {
		return errTxnNotFound
	}
	if m.active.ID != txnID {
		return errTxnNotFound
	}
	if m.clk.Now().UTC().After(m.active.ExpiresAt) {
		m.cleanupLocked()
		return errTxnExpired
	}
	return nil
}

// computeMerged builds the merged config from the current effective config
// plus all accumulated patches. Must be called with mu held.
func (m *configTxnManager) computeMerged(ctx context.Context) (*ports.BridgeConfig, error) {
	// Base the merge on the ON-DISK config -- the same source of truth the
	// commit-time CAS reads (readDiskVersion) and Begin baselines against.
	// Basing it on the in-memory applied config (configProvider) instead lets
	// a disk edit that the watcher has not yet applied be silently clobbered:
	// the version-only CAS compares disk versions and passes, but the merged
	// CONTENT is computed from stale memory, so the operator's newer file is
	// overwritten. Reading disk here keeps version AND content consistent.
	base, err := m.store.Load(ctx)
	if err != nil {
		if !isConfigNotFound(err) {
			return nil, fmt.Errorf("config txn: load disk config: %w", err)
		}
		// First-write semantics: nothing on disk yet (baseVersion==0). Fall
		// back to the in-memory config so a fresh deployment can still stage
		// and commit its initial configuration.
		base = m.configProvider()
	}
	if base == nil {
		return nil, fmt.Errorf("no current config available")
	}

	// A zero-patch transaction would otherwise return configProvider()'s
	// shared pointer (the live appliedRef object). Callers own the result --
	// Commit mutates Version in place before writing -- so return a copy to
	// avoid mutating, and racing concurrent GET /config reads against, the
	// live config. The >=1-patch path already yields a fresh struct from
	// Merge (DefaultMerge copies base), so it is already safe.
	if len(m.active.patches) == 0 {
		clone := *base
		return &clone, nil
	}

	result := base
	for i, patch := range m.active.patches {
		merged, err := m.store.Merge(ctx, result, patch)
		if err != nil {
			return nil, fmt.Errorf("merge patch %d: %w", i, err)
		}
		result = merged
	}
	return result, nil
}

// cleanup stops the timer and clears the active transaction.
func (m *configTxnManager) cleanup() {
	m.cleanupLocked()
}

// cleanupLocked stops the timer and clears the active transaction.
// Must be called with mu held.
func (m *configTxnManager) cleanupLocked() {
	if m.timeoutTimer != nil {
		m.timeoutTimer.Stop()
		m.timeoutTimer = nil
	}
	if m.timeoutCancel != nil {
		close(m.timeoutCancel)
		m.timeoutCancel = nil
	}
	m.active = nil
}

// generateTxnID returns a random 16-character hex string. It panics on
// crypto/rand failure, matching the codebase's other ID generators: a system
// that cannot produce randomness must not silently fall back to a predictable,
// collidable timestamp-based ID for a value used in optimistic-concurrency
// control.
func generateTxnID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("httpapi: crypto/rand failed generating txn id: %v", err))
	}
	return hex.EncodeToString(b)
}
