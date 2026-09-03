package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	// (see). It is a deployment-configuration condition, not a
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
	// non-CAS store by default (see).
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

// Commit validates the final merged config and writes it to the config
// file using optimistic concurrency control. It reads the on-disk config,
// verifies that its version matches the version captured when the
// transaction was created (check-and-set), increments the version, and
// writes. Returns errVersionConflict if another instance committed a
// different version in the meantime. The transaction is cleaned up on
// success.
//
// Note: on network filesystems (NFS/EFS) a plain file-backed store's
// check-read and write are not perfectly atomic. When the configured
// ports.ConfigStore also implements ports.ConditionalConfigStore the commit
// uses SaveIfVersion for a genuinely atomic cross-instance CAS (e.g. the
// DynamoDB-backed config profile); otherwise it degrades to the best-effort
// read-check-Save with the documented last-writer-wins caveat.
func (m *configTxnManager) Commit(ctx context.Context, txnID string) (int, error) {
	// Serialize the entire commit pipeline (durable write + out-of-lock apply +
	// rollback). See the commitMu field comment: this prevents a concurrent
	// commit from landing a durable write or running a second runtime swap
	// inside this commit's apply window, which a blind rollback would otherwise
	// clobber. m.mu is intentionally NOT held across the apply.
	m.commitMu.Lock()
	defer m.commitMu.Unlock()

	merged, newVersion, prior, applier, err := m.commitDurable(ctx, txnID)
	if err != nil {
		return 0, err
	}

	// Apply OUTSIDE the manager lock and on a context DETACHED from the request.
	// The durable write already succeeded; the build/connect/drain that follows
	// can take tens of seconds, and holding m.mu across it would block every
	// other txn endpoint. Detaching from the request context means a client
	// disconnect or write-deadline can no longer cancel the runtime swap
	// mid-flight AFTER the durable write — the durable commit and the in-band
	// apply must not be torn apart.
	if applier != nil {
		applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitApplyTimeout)
		defer cancel()
		if applyErr := applier(applyCtx, merged); applyErr != nil {
			// ports.ErrApplyInFlight is the "committed, NOT confirmed applied, DO
			// NOT roll back" terminal signal: the runtime accepted cfg but its
			// running state is not confirmed (the swap is still in-flight past
			// the apply deadline, or the bridge is paused/shutting down and
			// recorded cfg for a later resume). In every such case cfg is (or
			// will become) the durable desired state, so rolling the durable
			// write back would fight the runtime. Keep the committed config on
			// disk and surface committed_not_applied; the file watcher / next
			// swap converges the observable state. This is the reconciliation
			// path for a crash after the durable write leaves an unapplied
			// config: the durable write is deliberately RETAINED so a restart
			// recovers the committed config instead of losing it to a rollback.
			if errors.Is(applyErr, ports.ErrApplyInFlight) {
				return newVersion, fmt.Errorf("%w: version %d is committed to disk; apply is in-flight and the running state is not confirmed (not rolled back): %w",
					errConfigApplyFailed, newVersion, applyErr)
			}
			return m.rollbackAfterApplyFailure(ctx, newVersion, prior, applyErr)
		}
	}

	return newVersion, nil
}

// rollbackAfterApplyFailure restores the previous on-disk config after a failed
// in-band apply so a process restart recovers the last good config instead of
// crash-looping on the rejected one (the "committed_not_applied restart bomb").
// It reports rolled_back on success. When there is no previous config (first
// write) or the restore write itself fails, disk still holds the rejected
// config, so it falls back to committed_not_applied and the operator reconciles.
func (m *configTxnManager) rollbackAfterApplyFailure(ctx context.Context, newVersion int, prior *ports.BridgeConfig, applyErr error) (int, error) {
	if prior == nil {
		// First write: nothing to roll back to. Disk holds the new (rejected)
		// config; report committed_not_applied honestly.
		return newVersion, fmt.Errorf("%w: version %d is on disk but the running runtime did not converge: %w",
			errConfigApplyFailed, newVersion, applyErr)
	}

	// Restore on a FRESH context: the apply context may already be past its
	// deadline (a timed-out apply), which would fail the restore write. The
	// restore mirrors the commit's write discipline (CAS when the store
	// supports it) so the rollback cannot clobber a concurrent commit.
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitApplyTimeout)
	defer cancel()
	if restoreErr := m.restoreConfig(restoreCtx, prior, newVersion); restoreErr != nil {
		if m.logger != nil {
			m.logger.Error("config commit: apply failed AND rollback did not restore the previous config (write failed or a concurrent commit advanced the version); disk and runtime may diverge",
				"failed_version", newVersion, "restore_error", restoreErr, "apply_error", applyErr)
		}
		return newVersion, fmt.Errorf("%w: version %d is on disk but the running runtime did not converge, and rollback did not restore the previous config (%v): %w",
			errConfigApplyFailed, newVersion, restoreErr, applyErr)
	}

	if m.logger != nil {
		m.logger.Warn("config commit: apply failed; on-disk config rolled back to previous version",
			"rejected_version", newVersion, "restored_version", prior.Version, "apply_error", applyErr)
	}
	return prior.Version, fmt.Errorf("%w: apply of version %d failed; disk restored to version %d: %w",
		errConfigRolledBack, newVersion, prior.Version, applyErr)
}

// restoreConfig writes prior back to the store during a rollback, mirroring
// commitDurable's write discipline. Against a ports.ConditionalConfigStore it
// uses SaveIfVersion(prior, committedVersion) so the rollback only lands while
// THIS transaction's just-committed version is still the latest on disk; if a
// concurrent writer already advanced past it, the CAS refuses rather than
// clobbering that acknowledged commit — closing the same lost-update window on
// the rollback path that the forward commit closes. Stores without the
// capability keep the plain Save (documented single-writer last-writer-wins).
func (m *configTxnManager) restoreConfig(ctx context.Context, prior *ports.BridgeConfig, committedVersion int) error {
	if cas, ok := m.store.(ports.ConditionalConfigStore); ok {
		return cas.SaveIfVersion(ctx, prior, committedVersion)
	}
	return m.store.Save(ctx, prior)
}

// commitDurable performs the transactional, DURABLE portion of a commit under
// the manager lock: compute+validate the merged config, guard against plugin-
// option loss, CAS the on-disk version, Save, then clear the transaction and
// stop its TTL timer. It returns the merged config, the new version, the PRIOR
// on-disk config (for rollback if the post-lock apply fails; nil on first
// write), and the applier to run AFTER the lock is released (nil when none is
// wired). The lock is intentionally NOT held across the apply — see Commit.
func (m *configTxnManager) commitDurable(ctx context.Context, txnID string) (*ports.BridgeConfig, int, *ports.BridgeConfig, func(context.Context, *ports.BridgeConfig) error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, 0, nil, nil, err
	}

	merged, err := m.computeMerged(ctx)
	if err != nil {
		return nil, 0, nil, nil, err
	}

	if _, valErr := m.store.Validate(ctx, merged); valErr != nil {
		return nil, 0, nil, nil, valErr
	}

	// Guard the CRITICAL config-corruption class: a PATCH that touches an
	// existing plugin-config-bearing entry must never drop that entry's typed
	// Config (its broker URL/credentials/options live only in the typed
	// Config; the disk projection writes options from Config alone, so a nil
	// Config erases them permanently). If any entry that HAD a non-nil Config
	// in the running config lost it in the merge, refuse to persist. This is
	// belt-and-suspenders behind the merge layer (which preserves Config on
	// scalar PATCHes) and also correctly rejects transport changes attempted
	// via PATCH, which cannot carry replacement options.
	if err := guardNoConfigLoss(m.configProvider(), merged); err != nil {
		return nil, 0, nil, nil, err
	}

	// CAS: read the current on-disk config once. Its version drives the
	// check-and-set, and the config itself is retained as the rollback target
	// if the post-lock apply fails. A missing file (fs.ErrNotExist) is
	// first-write: version 0 and no prior config to restore.
	prior, err := m.store.Load(ctx)
	var diskVersion int
	switch {
	case err == nil:
		diskVersion = prior.Version
	case errors.Is(err, fs.ErrNotExist):
		prior = nil
		diskVersion = 0
	default:
		return nil, 0, nil, nil, fmt.Errorf("config commit: read disk version: %w", err)
	}
	if diskVersion != m.active.baseVersion {
		return nil, 0, nil, nil, fmt.Errorf("%w: expected version %d but file has version %d; re-read the config and retry",
			errVersionConflict, m.active.baseVersion, diskVersion)
	}

	newVersion := diskVersion + 1
	merged.Version = newVersion

	// Persist with an ATOMIC compare-and-swap when the store supports it. A
	// plain read-check-Save (the version guard above followed by Save) has a
	// lost-update window between the version read and the write: two admin
	// instances both committing from version N against a shared backend each
	// pass the guard, and the second Save clobbers the first — last-writer-wins,
	// a silently dropped acknowledged commit (and a version regression should a
	// later rollback fire). ports.ConditionalConfigStore.SaveIfVersion closes
	// that window: it writes only if the store's CURRENTLY persisted version
	// still equals the transaction's baseVersion, so a concurrent commit that
	// advanced the version is rejected with shared.ErrVersionMismatch (surfaced
	// as errVersionConflict) instead of being overwritten. Stores that do NOT
	// implement the capability have no safe cross-instance write, so the plain
	// Save is taken ONLY when the operator asserted a single writer
	// (m.singleWriter); otherwise the commit fails closed with
	// errConfigStoreNotCAS rather than performing a silent last-writer-wins Save
	// (see).
	if cas, ok := m.store.(ports.ConditionalConfigStore); ok {
		if err := cas.SaveIfVersion(ctx, merged, m.active.baseVersion); err != nil {
			if errors.Is(err, shared.ErrVersionMismatch) {
				return nil, 0, nil, nil, fmt.Errorf("%w: expected version %d but a concurrent commit advanced the shared config; re-read the config and retry",
					errVersionConflict, m.active.baseVersion)
			}
			return nil, 0, nil, nil, fmt.Errorf("config write failed: %w", err)
		}
	} else if !m.singleWriter {
		// Fail closed: a non-CAS store on a possibly-multi-writer deployment
		// cannot serialize concurrent commits. The read-time version guard above
		// is NOT atomic with this write, so two admin instances that both read
		// version N would each pass it and the second plain Save would clobber
		// the first acknowledged commit (silent lost update;). Refuse
		// the durable write instead of performing it silently. The operator must
		// either wire a ports.ConditionalConfigStore (always safe) or assert a
		// single writer via Config.ConfigSingleWriter.
		return nil, 0, nil, nil, fmt.Errorf("%w: the configured config store does not support compare-and-swap saves; enable it with a ports.ConditionalConfigStore or set Config.ConfigSingleWriter to assert a single admin writer",
			errConfigStoreNotCAS)
	} else if err := m.store.Save(ctx, merged); err != nil {
		// Single-writer asserted: a plain Save is safe because no peer can
		// clobber this write. This is the documented single-writer LWW path.
		return nil, 0, nil, nil, fmt.Errorf("config write failed: %w", err)
	}

	merged = m.projectionOf(ctx, merged)

	// The durable write and version transition are complete, so the transaction
	// is logically done regardless of the apply outcome. Clear it (and stop the
	// TTL timer) NOW, before releasing the lock, so a long apply neither blocks
	// other txn endpoints nor races the expiry timer into a spurious rollback.
	applier := m.applier
	m.cleanupLocked()
	return merged, newVersion, prior, applier, nil
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
// reports the underlying source does not yet exist (errors.Is the
// stdlib fs.ErrNotExist sentinel); the txn API signals "first-write"
// semantics via baseVersion=0.
func (m *configTxnManager) readDiskVersion(ctx context.Context) (int, error) {
	diskCfg, err := m.store.Load(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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
		if !errors.Is(err, fs.ErrNotExist) {
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
