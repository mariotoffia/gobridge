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
	"github.com/mariotoffia/gobridge/ports"
)

// Default transaction TTL when the client does not specify one.
const defaultTxnTTL = 5 * time.Minute

// Maximum allowed transaction TTL.
const maxTxnTTL = 30 * time.Minute

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
	// to disk but the in-band apply to the running runtime failed. The write
	// is NOT rolled back; the operator must reconcile. Distinct so the handler
	// can report "committed to disk but not applied" rather than a generic
	// failure.
	errConfigApplyFailed = errors.New("config committed but apply failed")
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
	mu             sync.Mutex
	active         *ConfigTransaction
	store          ports.ConfigStore
	configProvider func() *ports.BridgeConfig
	applier        func(ctx context.Context, cfg *ports.BridgeConfig) error
	logger         *slog.Logger
	clk            clock.Clock
	timeoutTimer   clock.Timer
	timeoutCancel  chan struct{}
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

// Preview returns the current merged state of the transaction without
// adding a new patch.
func (m *configTxnManager) Preview(ctx context.Context, txnID string) (*ports.BridgeConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, err
	}

	if m.active.merged != nil {
		return m.active.merged, nil
	}

	merged, err := m.computeMerged(ctx)
	if err != nil {
		return nil, err
	}
	m.active.merged = merged
	return merged, nil
}

// Commit validates the final merged config and writes it to the config
// file using optimistic concurrency control. It reads the on-disk config,
// verifies that its version matches the version captured when the
// transaction was created (check-and-set), increments the version, and
// writes. Returns errVersionConflict if another instance committed a
// different version in the meantime. The transaction is cleaned up on
// success.
//
// Note: on network filesystems (NFS/EFS) the check-read and write are
// not perfectly atomic. For truly concurrent config management, use the
// DynamoDB-backed config profile instead.
func (m *configTxnManager) Commit(ctx context.Context, txnID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return 0, err
	}

	merged, err := m.computeMerged(ctx)
	if err != nil {
		return 0, err
	}

	if _, valErr := m.store.Validate(ctx, merged); valErr != nil {
		return 0, valErr
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
		return 0, err
	}

	// CAS: read current on-disk version and compare with our base.
	diskVersion, err := m.readDiskVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("config commit: read disk version: %w", err)
	}
	if diskVersion != m.active.baseVersion {
		return 0, fmt.Errorf("%w: expected version %d but file has version %d; re-read the config and retry",
			errVersionConflict, m.active.baseVersion, diskVersion)
	}

	newVersion := diskVersion + 1
	merged.Version = newVersion

	if err := m.store.Save(ctx, merged); err != nil {
		return 0, fmt.Errorf("config write failed: %w", err)
	}

	// Apply to the running runtime in-band when an applier is wired. The
	// durable write already succeeded, so an apply failure is reported to the
	// operator (they must reconcile) rather than the API falsely claiming the
	// runtime converged while it diverged. When no applier is wired,
	// application is delegated to the file watcher (historical behavior).
	if m.applier != nil {
		if err := m.applier(ctx, merged); err != nil {
			m.cleanup()
			return newVersion, fmt.Errorf("%w: version %d is on disk but the running runtime did not converge: %w", errConfigApplyFailed, newVersion, err)
		}
	}

	m.cleanup()
	return newVersion, nil
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

// Active returns the active transaction, or nil if none.
func (m *configTxnManager) Active() *ConfigTransaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
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
	base := m.configProvider()
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
