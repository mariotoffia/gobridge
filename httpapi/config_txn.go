package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/config"
)

// Default transaction TTL when the client does not specify one.
const defaultTxnTTL = 5 * time.Minute

// Maximum allowed transaction TTL.
const maxTxnTTL = 30 * time.Minute

// Sentinel errors returned by configTxnManager methods.
var (
	errTxnActive   = errors.New("another transaction is already active")
	errTxnNotFound = errors.New("transaction not found")
	errTxnExpired  = errors.New("transaction has expired")
)

// ConfigTransaction represents an in-progress configuration change.
type ConfigTransaction struct {
	ID         string    `json:"txn_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	PatchCount int       `json:"patch_count"`

	patches []*config.BridgeConfig
	merged  *config.BridgeConfig
}

// configTxnManager manages the single active config transaction.
// All methods are safe for concurrent use.
type configTxnManager struct {
	mu             sync.Mutex
	active         *ConfigTransaction
	configFilePath string
	configProvider func() *config.BridgeConfig
	logger         *slog.Logger
	timeoutTimer   *time.Timer
}

// newTxnManager creates a new transaction manager.
// path is the config file to write on commit.
// provider returns the current effective config (e.g. Supervisor.Config).
func newTxnManager(path string, provider func() *config.BridgeConfig, logger *slog.Logger) *configTxnManager {
	return &configTxnManager{
		configFilePath: path,
		configProvider: provider,
		logger:         logger,
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

	now := time.Now().UTC()
	txn := &ConfigTransaction{
		ID:        generateTxnID(),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	m.active = txn

	m.timeoutTimer = time.AfterFunc(ttl, m.expire)

	return txn, nil
}

// Patch applies a config overlay to the active transaction. It merges the
// overlay on top of the current effective config plus all previous patches,
// validates the result, and returns the merged preview along with any
// validation warnings. Returns an error if the merged config is invalid.
func (m *configTxnManager) Patch(txnID string, overlay *config.BridgeConfig) (*config.BridgeConfig, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, nil, err
	}

	m.active.patches = append(m.active.patches, overlay)
	m.active.PatchCount++
	m.active.merged = nil // invalidate cache

	merged, err := m.computeMerged()
	if err != nil {
		// Remove the bad patch so the transaction remains usable.
		m.active.patches = m.active.patches[:len(m.active.patches)-1]
		m.active.PatchCount--
		return nil, nil, err
	}

	warnings, valErr := config.ValidateWithWarnings(merged)
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
func (m *configTxnManager) Preview(txnID string) (*config.BridgeConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return nil, err
	}

	if m.active.merged != nil {
		return m.active.merged, nil
	}

	merged, err := m.computeMerged()
	if err != nil {
		return nil, err
	}
	m.active.merged = merged
	return merged, nil
}

// Commit validates the final merged config and atomically writes it to
// the config file. The transaction is cleaned up on success.
func (m *configTxnManager) Commit(txnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkTxn(txnID); err != nil {
		return err
	}

	merged, err := m.computeMerged()
	if err != nil {
		return err
	}

	if _, valErr := config.ValidateWithWarnings(merged); valErr != nil {
		return valErr
	}

	if err := config.WriteFile(m.configFilePath, merged); err != nil {
		return fmt.Errorf("config write failed: %w", err)
	}

	m.cleanup()
	return nil
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
func (m *configTxnManager) expire() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active == nil {
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
	if time.Now().UTC().After(m.active.ExpiresAt) {
		m.cleanupLocked()
		return errTxnExpired
	}
	return nil
}

// computeMerged builds the merged config from the current effective config
// plus all accumulated patches. Must be called with mu held.
func (m *configTxnManager) computeMerged() (*config.BridgeConfig, error) {
	base := m.configProvider()
	if base == nil {
		return nil, fmt.Errorf("no current config available")
	}

	result := base
	for i, patch := range m.active.patches {
		merged, err := config.DefaultMerge(result, patch)
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
	m.active = nil
}

// generateTxnID returns a random 16-character hex string.
func generateTxnID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID on rand failure.
		return fmt.Sprintf("txn-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
