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
	logger         *slog.Logger
	clk            clock.Clock
	timeoutTimer   clock.Timer
	timeoutCancel  chan struct{}
}

// newTxnManager creates a new transaction manager.
// store is the persistence boundary used for validate/merge/save/load.
// provider returns the current effective config (e.g. Supervisor.Config).
func newTxnManager(store ports.ConfigStore, provider func() *ports.BridgeConfig, logger *slog.Logger, clk clock.Clock) *configTxnManager {
	if clk == nil {
		clk = clock.System
	}
	return &configTxnManager{
		store:          store,
		configProvider: provider,
		logger:         logger,
		clk:            clk,
	}
}

// Begin starts a new config transaction. Returns errTxnActive if a
// transaction is already in progress. The ttl controls how long the
// transaction remains active before auto-rollback; zero uses the default.
func (m *configTxnManager) Begin(ttl time.Duration) (*ConfigTransaction, error) {
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

	baseVersion := 0
	if current := m.configProvider(); current != nil {
		baseVersion = current.Version
	}

	txn := &ConfigTransaction{
		ID:          generateTxnID(m.clk),
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

	m.cleanup()
	return newVersion, nil
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

// generateTxnID returns a random 16-character hex string.
func generateTxnID(clk clock.Clock) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID on rand failure.
		return fmt.Sprintf("txn-%d", clk.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
