package core

import (
	"context"
	"fmt"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// TargetRegistryImpl implements types.TargetRegistry.
// It manages TargetFactory instances and creates Targets from configuration.
type TargetRegistryImpl struct {
	mu        sync.RWMutex
	factories map[types.TransportType]types.TargetFactory
}

// Ensure TargetRegistryImpl implements types.TargetRegistry
var _ types.TargetRegistry = (*TargetRegistryImpl)(nil)

// NewTargetRegistry creates a new TargetRegistry.
func NewTargetRegistry() *TargetRegistryImpl {
	return &TargetRegistryImpl{
		factories: make(map[types.TransportType]types.TargetFactory),
	}
}

// RegisterFactory registers a factory for creating targets.
// If a factory for the same transport type already exists, it is replaced.
func (r *TargetRegistryImpl) RegisterFactory(factory types.TargetFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, transportType := range factory.SupportedTransports() {
		r.factories[transportType] = factory
	}
}

// CreateTarget creates a target using the appropriate registered factory.
// Returns an error if no factory is registered for the target's transport type.
func (r *TargetRegistryImpl) CreateTarget(ctx context.Context, config types.TargetConfig) (types.Target, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	transportType := config.GetTransportType()
	factory, ok := r.factories[transportType]
	if !ok {
		return nil, types.ErrNotFound.
			With("transport", string(transportType)).
			WithMessage(fmt.Sprintf("no target factory registered for transport type: %s", transportType))
	}

	return factory.CreateTarget(ctx, config)
}

// HasFactory returns true if a factory is registered for the given transport type.
func (r *TargetRegistryImpl) HasFactory(transportType types.TransportType) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.factories[transportType]
	return ok
}

// RegisteredTransports returns a list of all registered transport types.
func (r *TargetRegistryImpl) RegisteredTransports() []types.TransportType {
	r.mu.RLock()
	defer r.mu.RUnlock()

	transports := make([]types.TransportType, 0, len(r.factories))
	for t := range r.factories {
		transports = append(transports, t)
	}
	return transports
}

