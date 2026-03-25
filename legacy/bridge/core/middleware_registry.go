package core

import (
	"fmt"
	"sync"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// MiddlewareFactory creates Middleware instances.
// This allows middlewares to be created with different configurations
// while using the same underlying implementation.
type MiddlewareFactory func(config map[string]any) (types.Middleware, error)

// MiddlewareRegistry manages middleware factories and creates middleware chains.
// Unlike sources and targets, middlewares are registered by name and can be
// instantiated with optional configuration.
type MiddlewareRegistry struct {
	mu sync.RWMutex

	// factories maps middleware names to their factory functions.
	factories map[string]MiddlewareFactory

	// singletons maps middleware names to pre-created instances.
	// Used for stateless middlewares that can be shared.
	singletons map[string]types.Middleware
}

// NewMiddlewareRegistry creates a new MiddlewareRegistry.
func NewMiddlewareRegistry() *MiddlewareRegistry {
	return &MiddlewareRegistry{
		factories:  make(map[string]MiddlewareFactory),
		singletons: make(map[string]types.Middleware),
	}
}

// RegisterFactory registers a factory function for creating middlewares.
// The name should be unique and is used to reference the middleware in configuration.
func (r *MiddlewareRegistry) RegisterFactory(name string, factory MiddlewareFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.factories[name] = factory
}

// Register registers a pre-created middleware instance as a singleton.
// This is useful for stateless middlewares that don't need per-pipeline instances.
func (r *MiddlewareRegistry) Register(middleware types.Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.singletons[middleware.Name()] = middleware
}

// Get returns a middleware by name.
// If a singleton exists, it is returned. Otherwise, the factory is called with nil config.
func (r *MiddlewareRegistry) Get(name string) (types.Middleware, error) {
	return r.GetWithConfig(name, nil)
}

// GetWithConfig returns a middleware by name, creating it with the given configuration.
// If a singleton exists and config is nil, the singleton is returned.
// Otherwise, the factory is called to create a new instance.
func (r *MiddlewareRegistry) GetWithConfig(name string, config map[string]any) (types.Middleware, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check for singleton first (only if no config provided)
	if config == nil {
		if mw, ok := r.singletons[name]; ok {
			return mw, nil
		}
	}

	// Check for factory
	factory, ok := r.factories[name]
	if !ok {
		return nil, types.ErrNotFound.
			With("middleware", name).
			WithMessage(fmt.Sprintf("no middleware registered with name: %s", name))
	}

	return factory(config)
}

// CreateChain creates a middleware chain from the given middleware names.
// The middlewares are added to the chain in the order specified.
func (r *MiddlewareRegistry) CreateChain(names ...string) (*types.MiddlewareChain, error) {
	middlewares := make([]types.Middleware, 0, len(names))

	for _, name := range names {
		mw, err := r.Get(name)
		if err != nil {
			return nil, err
		}
		middlewares = append(middlewares, mw)
	}

	return types.NewMiddlewareChain(middlewares...), nil
}

// CreateChainWithConfigs creates a middleware chain with per-middleware configuration.
// Each MiddlewareSpec specifies the middleware name and optional configuration.
func (r *MiddlewareRegistry) CreateChainWithConfigs(specs ...MiddlewareSpec) (*types.MiddlewareChain, error) {
	middlewares := make([]types.Middleware, 0, len(specs))

	for _, spec := range specs {
		mw, err := r.GetWithConfig(spec.Name, spec.Config)
		if err != nil {
			return nil, err
		}
		middlewares = append(middlewares, mw)
	}

	return types.NewMiddlewareChain(middlewares...), nil
}

// HasMiddleware returns true if a middleware with the given name is registered.
func (r *MiddlewareRegistry) HasMiddleware(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, hasSingleton := r.singletons[name]
	_, hasFactory := r.factories[name]
	return hasSingleton || hasFactory
}

// RegisteredNames returns a list of all registered middleware names.
func (r *MiddlewareRegistry) RegisteredNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make(map[string]struct{})
	for name := range r.singletons {
		names[name] = struct{}{}
	}
	for name := range r.factories {
		names[name] = struct{}{}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	return result
}

// MiddlewareSpec specifies a middleware and its configuration for chain creation.
type MiddlewareSpec struct {
	// Name is the registered middleware name.
	Name string
	// Config is optional configuration for this middleware instance.
	Config map[string]any
}
