package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ConfigChangeHandlerImpl implements types.ConfigChangeHandler.
// It processes configuration changes and coordinates with the connection registry
// and lifecycle coordinators for atomic operations.
type ConfigChangeHandlerImpl struct {
	// connectionRegistry for managing connections
	connectionRegistry types.ConnectionRegistry

	// sourceRegistry for creating sources
	sourceRegistry types.SourceRegistry

	// targetRegistry for creating targets
	targetRegistry types.TargetRegistry

	// mu protects internal state
	mu sync.RWMutex
}

// ConfigChangeHandlerOption configures a ConfigChangeHandlerImpl.
type ConfigChangeHandlerOption func(*ConfigChangeHandlerImpl)

// WithConnectionRegistry sets the connection registry for the handler.
func ConfigHandlerWithConnectionRegistry(registry types.ConnectionRegistry) ConfigChangeHandlerOption {
	return func(h *ConfigChangeHandlerImpl) {
		h.connectionRegistry = registry
	}
}

// WithSourceRegistry sets the source registry for the handler.
func ConfigHandlerWithSourceRegistry(registry types.SourceRegistry) ConfigChangeHandlerOption {
	return func(h *ConfigChangeHandlerImpl) {
		h.sourceRegistry = registry
	}
}

// WithTargetRegistry sets the target registry for the handler.
func ConfigHandlerWithTargetRegistry(registry types.TargetRegistry) ConfigChangeHandlerOption {
	return func(h *ConfigChangeHandlerImpl) {
		h.targetRegistry = registry
	}
}

// NewConfigChangeHandler creates a new ConfigChangeHandlerImpl.
func NewConfigChangeHandler(opts ...ConfigChangeHandlerOption) *ConfigChangeHandlerImpl {
	h := &ConfigChangeHandlerImpl{}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// HandleConnectionChange processes connection add/update/delete.
func (h *ConfigChangeHandlerImpl) HandleConnectionChange(ctx context.Context, change types.ConfigChange) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.connectionRegistry == nil {
		return fmt.Errorf("connection registry not configured")
	}

	switch change.Type {
	case types.ConfigChangeAdd:
		return h.handleConnectionAdd(ctx, change)
	case types.ConfigChangeUpdate:
		return h.handleConnectionUpdate(ctx, change)
	case types.ConfigChangeDelete:
		return h.handleConnectionDelete(ctx, change)
	default:
		return fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// handleConnectionAdd creates and starts a new connection.
func (h *ConfigChangeHandlerImpl) handleConnectionAdd(ctx context.Context, change types.ConfigChange) error {
	config, ok := change.Item.GetData().(types.ConnectionConfig)
	if !ok {
		return fmt.Errorf("invalid connection config data")
	}

	conn, err := h.connectionRegistry.CreateConnection(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to create connection: %w", err)
	}

	if err := conn.Start(ctx, config); err != nil {
		return fmt.Errorf("failed to start connection: %w", err)
	}

	return h.connectionRegistry.RegisterConnection(conn)
}

// handleConnectionUpdate updates connection settings.
func (h *ConfigChangeHandlerImpl) handleConnectionUpdate(ctx context.Context, change types.ConfigChange) error {
	settings, ok := change.Item.GetData().(types.ConnectionSettingsConfig)
	if !ok {
		return fmt.Errorf("invalid connection settings data")
	}

	// Get connection ID from partition key or settings
	connID := settings.GetID()

	conn, err := h.connectionRegistry.GetConnection(connID)
	if err != nil {
		return fmt.Errorf("connection not found: %w", err)
	}

	return conn.UpdateSettings(ctx, settings)
}

// handleConnectionDelete removes a connection after draining.
func (h *ConfigChangeHandlerImpl) handleConnectionDelete(ctx context.Context, change types.ConfigChange) error {
	// Extract connection ID from the item
	connID := change.Item.GetPartitionKey()

	conn, err := h.connectionRegistry.GetConnection(connID)
	if err != nil {
		return fmt.Errorf("connection not found: %w", err)
	}

	// Drain the connection first
	if err := conn.Drain(ctx); err != nil {
		return fmt.Errorf("failed to drain connection: %w", err)
	}

	// Close the connection
	if err := conn.Close(); err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}

	// Remove from registry
	return h.connectionRegistry.RemoveConnection(connID)
}

// HandleSourceChange processes source add/update/delete.
func (h *ConfigChangeHandlerImpl) HandleSourceChange(ctx context.Context, change types.ConfigChange) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Extract connection ID from partition key (e.g., "connection:mqtt-1")
	connID := extractConnectionID(change.Item.GetPartitionKey())

	conn, err := h.connectionRegistry.GetConnection(connID)
	if err != nil {
		return fmt.Errorf("connection not found: %w", err)
	}

	// Use lifecycle coordinator if available for atomic changes
	coordinator := conn.LifecycleCoordinator()
	if coordinator != nil {
		return h.handleSourceChangeAtomic(ctx, change, coordinator)
	}

	// Fall back to non-atomic handling
	return h.handleSourceChangeNonAtomic(ctx, change, conn)
}

// handleSourceChangeAtomic uses LifecycleCoordinator for atomic changes.
func (h *ConfigChangeHandlerImpl) handleSourceChangeAtomic(ctx context.Context, change types.ConfigChange, coordinator types.LifecycleCoordinator) error {
	txn, err := coordinator.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.SourceConfig)
		if !ok {
			_ = txn.Rollback()
			return fmt.Errorf("invalid source config data")
		}
		if err := txn.AddSource(config); err != nil {
			_ = txn.Rollback()
			return err
		}
	case types.ConfigChangeUpdate:
		config, ok := change.Item.GetData().(types.SourceConfig)
		if !ok {
			_ = txn.Rollback()
			return fmt.Errorf("invalid source config data")
		}
		sourceID := extractSourceID(change.Item.GetSortKey())
		if err := txn.UpdateSource(sourceID, config); err != nil {
			_ = txn.Rollback()
			return err
		}
	case types.ConfigChangeDelete:
		sourceID := extractSourceID(change.Item.GetSortKey())
		if err := txn.RemoveSource(sourceID); err != nil {
			_ = txn.Rollback()
			return err
		}
	}

	_, err = txn.Commit(ctx)
	return err
}

// handleSourceChangeNonAtomic handles source changes without coordination.
func (h *ConfigChangeHandlerImpl) handleSourceChangeNonAtomic(ctx context.Context, change types.ConfigChange, conn types.Connection) error {
	provider := conn.SourceProvider()
	if provider == nil {
		return fmt.Errorf("connection does not support source creation")
	}

	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.SourceConfig)
		if !ok {
			return fmt.Errorf("invalid source config data")
		}
		_, err := provider.CreateSource(ctx, config)
		return err
	default:
		return fmt.Errorf("non-atomic source %s not supported", change.Type)
	}
}

// HandleTargetChange processes target add/update/delete.
func (h *ConfigChangeHandlerImpl) HandleTargetChange(ctx context.Context, change types.ConfigChange) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Extract connection ID from partition key
	connID := extractConnectionID(change.Item.GetPartitionKey())

	conn, err := h.connectionRegistry.GetConnection(connID)
	if err != nil {
		return fmt.Errorf("connection not found: %w", err)
	}

	// Use lifecycle coordinator if available for atomic changes
	coordinator := conn.LifecycleCoordinator()
	if coordinator != nil {
		return h.handleTargetChangeAtomic(ctx, change, coordinator)
	}

	// Fall back to non-atomic handling
	return h.handleTargetChangeNonAtomic(ctx, change, conn)
}

// handleTargetChangeAtomic uses LifecycleCoordinator for atomic changes.
func (h *ConfigChangeHandlerImpl) handleTargetChangeAtomic(ctx context.Context, change types.ConfigChange, coordinator types.LifecycleCoordinator) error {
	txn, err := coordinator.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.TargetConfig)
		if !ok {
			_ = txn.Rollback()
			return fmt.Errorf("invalid target config data")
		}
		if err := txn.AddTarget(config); err != nil {
			_ = txn.Rollback()
			return err
		}
	case types.ConfigChangeUpdate:
		config, ok := change.Item.GetData().(types.TargetConfig)
		if !ok {
			_ = txn.Rollback()
			return fmt.Errorf("invalid target config data")
		}
		targetID := extractTargetID(change.Item.GetSortKey())
		if err := txn.UpdateTarget(targetID, config); err != nil {
			_ = txn.Rollback()
			return err
		}
	case types.ConfigChangeDelete:
		targetID := extractTargetID(change.Item.GetSortKey())
		if err := txn.RemoveTarget(targetID); err != nil {
			_ = txn.Rollback()
			return err
		}
	}

	_, err = txn.Commit(ctx)
	return err
}

// handleTargetChangeNonAtomic handles target changes without coordination.
func (h *ConfigChangeHandlerImpl) handleTargetChangeNonAtomic(ctx context.Context, change types.ConfigChange, conn types.Connection) error {
	provider := conn.TargetProvider()
	if provider == nil {
		return fmt.Errorf("connection does not support target creation")
	}

	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.TargetConfig)
		if !ok {
			return fmt.Errorf("invalid target config data")
		}
		_, err := provider.CreateTarget(ctx, config)
		return err
	default:
		return fmt.Errorf("non-atomic target %s not supported", change.Type)
	}
}

// HandleBatchChanges processes multiple changes atomically when possible.
func (h *ConfigChangeHandlerImpl) HandleBatchChanges(ctx context.Context, changes []types.ConfigChange) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Group changes by connection
	connectionChanges := make(map[string][]types.ConfigChange)
	for _, change := range changes {
		connID := extractConnectionID(change.Item.GetPartitionKey())
		connectionChanges[connID] = append(connectionChanges[connID], change)
	}

	// Process each connection's changes
	for connID, connChanges := range connectionChanges {
		if err := h.processBatchForConnection(ctx, connID, connChanges); err != nil {
			return fmt.Errorf("failed to process batch for connection %s: %w", connID, err)
		}
	}

	return nil
}

// processBatchForConnection processes all changes for a single connection atomically.
func (h *ConfigChangeHandlerImpl) processBatchForConnection(ctx context.Context, connID string, changes []types.ConfigChange) error {
	conn, err := h.connectionRegistry.GetConnection(connID)
	if err != nil {
		// Handle connection-level changes first
		for _, change := range changes {
			if change.Item.GetType() == types.ConfigItemTypeConnection {
				if err := h.HandleConnectionChange(ctx, change); err != nil {
					return err
				}
			}
		}
		// Try to get connection again after potential creation
		conn, err = h.connectionRegistry.GetConnection(connID)
		if err != nil {
			return fmt.Errorf("connection not found: %w", err)
		}
	}

	coordinator := conn.LifecycleCoordinator()
	if coordinator == nil {
		// Fall back to individual processing
		for _, change := range changes {
			switch change.Item.GetType() {
			case types.ConfigItemTypeSource:
				if err := h.handleSourceChangeNonAtomic(ctx, change, conn); err != nil {
					return err
				}
			case types.ConfigItemTypeTarget:
				if err := h.handleTargetChangeNonAtomic(ctx, change, conn); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Use atomic transaction for all source/target changes
	txn, err := coordinator.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	for _, change := range changes {
		switch change.Item.GetType() {
		case types.ConfigItemTypeSource:
			if err := h.applySourceChangeToTxn(change, txn); err != nil {
				_ = txn.Rollback()
				return err
			}
		case types.ConfigItemTypeTarget:
			if err := h.applyTargetChangeToTxn(change, txn); err != nil {
				_ = txn.Rollback()
				return err
			}
		}
	}

	_, err = txn.Commit(ctx)
	return err
}

// applySourceChangeToTxn applies a source change to a transaction.
func (h *ConfigChangeHandlerImpl) applySourceChangeToTxn(change types.ConfigChange, txn types.LifecycleTransaction) error {
	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.SourceConfig)
		if !ok {
			return fmt.Errorf("invalid source config data")
		}
		return txn.AddSource(config)
	case types.ConfigChangeUpdate:
		config, ok := change.Item.GetData().(types.SourceConfig)
		if !ok {
			return fmt.Errorf("invalid source config data")
		}
		sourceID := extractSourceID(change.Item.GetSortKey())
		return txn.UpdateSource(sourceID, config)
	case types.ConfigChangeDelete:
		sourceID := extractSourceID(change.Item.GetSortKey())
		return txn.RemoveSource(sourceID)
	default:
		return fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// applyTargetChangeToTxn applies a target change to a transaction.
func (h *ConfigChangeHandlerImpl) applyTargetChangeToTxn(change types.ConfigChange, txn types.LifecycleTransaction) error {
	switch change.Type {
	case types.ConfigChangeAdd:
		config, ok := change.Item.GetData().(types.TargetConfig)
		if !ok {
			return fmt.Errorf("invalid target config data")
		}
		return txn.AddTarget(config)
	case types.ConfigChangeUpdate:
		config, ok := change.Item.GetData().(types.TargetConfig)
		if !ok {
			return fmt.Errorf("invalid target config data")
		}
		targetID := extractTargetID(change.Item.GetSortKey())
		return txn.UpdateTarget(targetID, config)
	case types.ConfigChangeDelete:
		targetID := extractTargetID(change.Item.GetSortKey())
		return txn.RemoveTarget(targetID)
	default:
		return fmt.Errorf("unknown change type: %s", change.Type)
	}
}

// extractConnectionID extracts connection ID from partition key.
// Expected format: "connection:{id}"
func extractConnectionID(partitionKey string) string {
	const prefix = "connection:"
	if len(partitionKey) > len(prefix) && partitionKey[:len(prefix)] == prefix {
		return partitionKey[len(prefix):]
	}
	return partitionKey
}

// extractSourceID extracts source ID from sort key.
// Expected format: "source:{id}"
func extractSourceID(sortKey string) string {
	const prefix = "source:"
	if len(sortKey) > len(prefix) && sortKey[:len(prefix)] == prefix {
		return sortKey[len(prefix):]
	}
	return sortKey
}

// extractTargetID extracts target ID from sort key.
// Expected format: "target:{id}"
func extractTargetID(sortKey string) string {
	const prefix = "target:"
	if len(sortKey) > len(prefix) && sortKey[:len(prefix)] == prefix {
		return sortKey[len(prefix):]
	}
	return sortKey
}

// Ensure ConfigChangeHandlerImpl implements types.ConfigChangeHandler
var _ types.ConfigChangeHandler = (*ConfigChangeHandlerImpl)(nil)
