package registry

import (
	"context"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// TransportCreatorFunc is a function type that creates a Transport based on the provided configuration.
type TransportCreatorFunc func(ctx context.Context, config types.TransportConfig) (types.Transport, error)

// TransportRegistryImpl is a concrete implementation of the `types.TransportRegistry` interface.
type TransportRegistryImpl struct {
	mu *sync.RWMutex
	// transports holds the registered transports mapped by their unique IDs.
	transports map[string]types.Transport
	// creators holds the registered creator functions for different transport types.
	creators map[types.TransportType]TransportCreatorFunc
}

// RegisterTransport adds a server to the registry.
func (r *TransportRegistryImpl) RegisterTransport(server types.Transport) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.transports[server.GetID()] = server
	return nil
}

// GetTransport retrieves a server by its unique ID.
func (r *TransportRegistryImpl) GetTransport(id string) (types.Transport, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	server, exists := r.transports[id]

	if !exists {
		return nil, types.ErrNotFound
	}

	return server, nil
}

// ListTransports returns a list of all registered transports.
func (r *TransportRegistryImpl) ListTransports() ([]types.Transport, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	transports := make([]types.Transport, 0, len(r.transports))

	for _, server := range r.transports {
		transports = append(transports, server)
	}

	return transports, nil
}

// RemoveTransport removes a transport from the registry by its unique ID.
func (r *TransportRegistryImpl) RemoveTransport(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.transports[id]; !exists {
		return types.ErrNotFound
	}

	delete(r.transports, id)
	return nil
}

// CreateTransport creates a new transport instance based on the provided configuration.
func (r *TransportRegistryImpl) CreateTransport(ctx context.Context, config types.TransportConfig) (types.Transport, error) {
	r.mu.RLock()
	creator, exists := r.creators[config.GetTransportType()]
	r.mu.RUnlock()

	if !exists {
		return nil, types.ErrNotFound
	}

	return creator(ctx, config)
}
