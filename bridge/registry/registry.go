package registry

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ConnectionCreatorFunc is a function type that creates a Connection based on the provided configuration.
type ConnectionCreatorFunc func(ctx context.Context, config types.ConnectionConfig) (types.Connection, error)

// ConnectionRegistryImpl is a concrete implementation of the `types.ConnectionRegistry` interface.
type ConnectionRegistryImpl struct {
	mu *sync.RWMutex
	// connections holds the registered connections mapped by their unique IDs.
	connections map[string]types.Connection
	// creators holds the registered creator functions for different connection types.
	creators map[types.TransportType]ConnectionCreatorFunc
}

// RegisterConnection adds a connection to the registry.
func (r *ConnectionRegistryImpl) RegisterConnection(connection types.Connection) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connections[connection.GetID()] = connection
	return nil
}

// GetConnection retrieves a connection by its unique ID.
func (r *ConnectionRegistryImpl) GetConnection(id string) (types.Connection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	connection, exists := r.connections[id]

	if !exists {
		return nil, types.ErrNotFound
	}

	return connection, nil
}

// ListConnections returns a list of all registered connections.
func (r *ConnectionRegistryImpl) ListConnections() ([]types.Connection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return slices.Collect(maps.Values(r.connections)), nil
}

// RemoveConnection removes a connection from the registry by its unique ID.
func (r *ConnectionRegistryImpl) RemoveConnection(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.connections[id]; !exists {
		return types.ErrNotFound
	}

	delete(r.connections, id)
	return nil
}

// CreateConnection creates a new connection instance based on the provided configuration.
func (r *ConnectionRegistryImpl) CreateConnection(ctx context.Context, config types.ConnectionConfig) (types.Connection, error) {
	r.mu.RLock()
	creator, exists := r.creators[config.GetTransportType()]
	r.mu.RUnlock()

	if !exists {
		return nil, types.ErrNotFound
	}

	// Create the connection using the registered creator function.
	return creator(ctx, config)
}

// ============================================================================
// ConnectionAdminRegistry Implementation (Optional)
// ============================================================================

// UpdateConnection updates an existing connection's settings.
// Coordinates drain/reconnect if RequiresReconnect() is true.
// Returns ErrNotFound if the connection doesn't exist.
func (r *ConnectionRegistryImpl) UpdateConnection(ctx context.Context, id string, settings types.ConnectionSettingsConfig) error {
	r.mu.RLock()
	conn, exists := r.connections[id]
	r.mu.RUnlock()

	if !exists {
		return types.ErrNotFound
	}

	return conn.UpdateSettings(ctx, settings)
}

// DeleteConnection removes a connection after draining and closing it.
// Returns ErrNotFound if the connection doesn't exist.
func (r *ConnectionRegistryImpl) DeleteConnection(ctx context.Context, id string) error {
	r.mu.Lock()
	conn, exists := r.connections[id]
	if !exists {
		r.mu.Unlock()
		return types.ErrNotFound
	}
	delete(r.connections, id)
	r.mu.Unlock()

	// Drain and close the connection outside the lock
	if err := conn.Drain(ctx); err != nil {
		// Log but continue with close
		_ = err
	}

	return conn.Close()
}

// GetOrCreateConnection gets an existing connection or creates a new one.
// Useful for ensuring idempotent operations.
func (r *ConnectionRegistryImpl) GetOrCreateConnection(ctx context.Context, config types.ConnectionConfig) (types.Connection, error) {
	id := config.GetID()

	// Check if connection already exists
	r.mu.RLock()
	conn, exists := r.connections[id]
	r.mu.RUnlock()

	if exists {
		return conn, nil
	}

	// Create new connection
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if conn, exists = r.connections[id]; exists {
		return conn, nil
	}

	creator, exists := r.creators[config.GetTransportType()]
	if !exists {
		return nil, types.ErrNotFound
	}

	conn, err := creator(ctx, config)
	if err != nil {
		return nil, err
	}

	r.connections[id] = conn
	return conn, nil
}

// ListConnectionIDs returns all connection IDs in the registry.
func (r *ConnectionRegistryImpl) ListConnectionIDs(ctx context.Context) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.connections))
	for id := range r.connections {
		ids = append(ids, id)
	}
	return ids, nil
}

// Ensure ConnectionRegistryImpl implements both interfaces
var _ types.ConnectionRegistry = (*ConnectionRegistryImpl)(nil)
var _ types.ConnectionAdminRegistry = (*ConnectionRegistryImpl)(nil)
