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

// ConnectionRegistryImpl is a concrete implementation of the `types.TransportRegistry` interface.
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

// AllConnection returns a list of all registered connections.
func (r *ConnectionRegistryImpl) AllConnection() ([]types.Connection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return slices.Collect(maps.Values(r.connections)), nil
}

// RemoveConnections removes a connection from the registry by its unique ID.
func (r *ConnectionRegistryImpl) RemoveConnections(id string) error {
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

	// TODO: Check if we should configure it as well? or maybe we do this externally?
	return creator(ctx, config)
}
