// Package core provides the runtime infrastructure for the bridge system.
// It includes registries for sources, targets, and middlewares, as well as
// the pipeline and route implementations that tie everything together.
package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// SourceRegistryImpl implements types.SourceRegistry.
// It manages SourceFactory instances and creates Sources from configuration.
type SourceRegistryImpl struct {
	mu        sync.RWMutex
	factories map[types.TransportType]types.SourceFactory
}

// Ensure SourceRegistryImpl implements types.SourceRegistry
var _ types.SourceRegistry = (*SourceRegistryImpl)(nil)

// NewSourceRegistry creates a new SourceRegistry.
func NewSourceRegistry() *SourceRegistryImpl {
	return &SourceRegistryImpl{
		factories: make(map[types.TransportType]types.SourceFactory),
	}
}

// RegisterFactory registers a factory for creating sources.
// If a factory for the same transport type already exists, it is replaced.
func (r *SourceRegistryImpl) RegisterFactory(factory types.SourceFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, transportType := range factory.SupportedTransports() {
		r.factories[transportType] = factory
	}
}

// CreateSource creates a source using the appropriate registered factory.
// Returns an error if no factory is registered for the source's transport type.
func (r *SourceRegistryImpl) CreateSource(ctx context.Context, config types.SourceConfig) (types.Source, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	transportType := config.GetTransportType()
	factory, ok := r.factories[transportType]
	if !ok {
		return nil, types.ErrNotFound.
			With("transport", string(transportType)).
			WithMessage(fmt.Sprintf("no source factory registered for transport type: %s", transportType))
	}

	return factory.CreateSource(ctx, config)
}

// HasFactory returns true if a factory is registered for the given transport type.
func (r *SourceRegistryImpl) HasFactory(transportType types.TransportType) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.factories[transportType]
	return ok
}

// RegisteredTransports returns a list of all registered transport types.
func (r *SourceRegistryImpl) RegisteredTransports() []types.TransportType {
	r.mu.RLock()
	defer r.mu.RUnlock()

	transports := make([]types.TransportType, 0, len(r.factories))
	for t := range r.factories {
		transports = append(transports, t)
	}
	return transports
}

