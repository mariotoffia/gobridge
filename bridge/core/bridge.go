package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Bridge is the main runtime that manages the lifecycle of all pipelines and routes.
// It coordinates sources, targets, and middlewares to create a message routing system.
type Bridge struct {
	// id is the unique identifier of this bridge instance
	id string

	// sourceRegistry manages source factories
	sourceRegistry types.SourceRegistry

	// targetRegistry manages target factories
	targetRegistry types.TargetRegistry

	// middlewareRegistry manages middleware factories
	middlewareRegistry *MiddlewareRegistry

	// configSource provides configuration (optional)
	configSource types.ConfigSource

	// pipelines holds all active pipelines by ID
	pipelines map[string]types.Pipeline

	// routes holds all active routes by ID
	routes map[string]types.Route

	// running indicates if the bridge is running
	running atomic.Bool

	// mu protects pipelines and routes maps
	mu sync.RWMutex

	// cancel cancels the bridge context
	cancel context.CancelFunc

	// wg tracks running goroutines
	wg sync.WaitGroup
}

// BridgeOption configures a Bridge.
type BridgeOption func(*Bridge)

// WithSourceRegistry sets the source registry.
func WithSourceRegistry(registry types.SourceRegistry) BridgeOption {
	return func(b *Bridge) {
		b.sourceRegistry = registry
	}
}

// WithTargetRegistry sets the target registry.
func WithTargetRegistry(registry types.TargetRegistry) BridgeOption {
	return func(b *Bridge) {
		b.targetRegistry = registry
	}
}

// WithMiddlewareRegistry sets the middleware registry.
func WithMiddlewareRegistry(registry *MiddlewareRegistry) BridgeOption {
	return func(b *Bridge) {
		b.middlewareRegistry = registry
	}
}

// WithConfigSource sets the configuration source.
func WithConfigSource(source types.ConfigSource) BridgeOption {
	return func(b *Bridge) {
		b.configSource = source
	}
}

// NewBridge creates a new Bridge instance.
func NewBridge(id string, opts ...BridgeOption) *Bridge {
	b := &Bridge{
		id:                 id,
		sourceRegistry:     NewSourceRegistry(),
		targetRegistry:     NewTargetRegistry(),
		middlewareRegistry: NewMiddlewareRegistry(),
		pipelines:          make(map[string]types.Pipeline),
		routes:             make(map[string]types.Route),
	}

	for _, opt := range opts {
		opt(b)
	}

	return b
}

// GetID returns the bridge identifier.
func (b *Bridge) GetID() string {
	return b.id
}

// SourceRegistry returns the source registry.
func (b *Bridge) SourceRegistry() types.SourceRegistry {
	return b.sourceRegistry
}

// TargetRegistry returns the target registry.
func (b *Bridge) TargetRegistry() types.TargetRegistry {
	return b.targetRegistry
}

// MiddlewareRegistry returns the middleware registry.
func (b *Bridge) MiddlewareRegistry() *MiddlewareRegistry {
	return b.middlewareRegistry
}

// Start starts the bridge and all registered pipelines/routes.
func (b *Bridge) Start(ctx context.Context) error {
	if !b.running.CompareAndSwap(false, true) {
		return errors.New("bridge already running")
	}

	ctx, b.cancel = context.WithCancel(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Start all routes
	for id, route := range b.routes {
		if err := route.Start(ctx); err != nil {
			// Stop already started routes
			b.stopAllLocked()
			b.running.Store(false)
			return fmt.Errorf("failed to start route %s: %w", id, err)
		}
	}

	// Start all standalone pipelines (not part of routes)
	for id, pipeline := range b.pipelines {
		if err := pipeline.Start(ctx); err != nil {
			// Stop already started pipelines and routes
			b.stopAllLocked()
			b.running.Store(false)
			return fmt.Errorf("failed to start pipeline %s: %w", id, err)
		}
	}

	// If we have a config source, start watching for changes
	if b.configSource != nil {
		b.wg.Add(1)
		go b.watchConfig(ctx)
	}

	return nil
}

// stopAllLocked stops all pipelines and routes. Caller must hold mu.
func (b *Bridge) stopAllLocked() {
	for _, pipeline := range b.pipelines {
		_ = pipeline.Close()
	}
	for _, route := range b.routes {
		_ = route.Close()
	}
}

// watchConfig watches for configuration changes and applies them.
func (b *Bridge) watchConfig(ctx context.Context) {
	defer b.wg.Done()

	changeCh, err := b.configSource.Watch(ctx)
	if err != nil {
		// Log error but don't crash
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case change, ok := <-changeCh:
			if !ok {
				return
			}
			b.handleConfigChange(ctx, change)
		}
	}
}

// handleConfigChange processes a configuration change.
func (b *Bridge) handleConfigChange(ctx context.Context, change types.ConfigChange) {
	// TODO: Implement dynamic configuration handling
	// For now, this is a placeholder for future implementation
	_ = ctx
	_ = change
}

// Stop gracefully stops the bridge and all pipelines/routes.
func (b *Bridge) Stop(ctx context.Context) error {
	if !b.running.CompareAndSwap(true, false) {
		return nil // Already stopped
	}

	// Cancel context
	if b.cancel != nil {
		b.cancel()
	}

	// Wait for goroutines
	b.wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()

	var errs []error

	// Stop all pipelines
	for id, pipeline := range b.pipelines {
		if err := pipeline.Close(); err != nil {
			errs = append(errs, fmt.Errorf("pipeline %s: %w", id, err))
		}
	}

	// Stop all routes
	for id, route := range b.routes {
		if err := route.Close(); err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

// Close is an alias for Stop with background context.
func (b *Bridge) Close() error {
	return b.Stop(context.Background())
}

// AddPipeline adds a pipeline to the bridge.
// Returns error if the bridge is running or if a pipeline with the same ID exists.
func (b *Bridge) AddPipeline(pipeline types.Pipeline) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot add pipeline to running bridge")
	}

	id := pipeline.GetID()
	if _, exists := b.pipelines[id]; exists {
		return fmt.Errorf("pipeline with ID %s already exists", id)
	}

	b.pipelines[id] = pipeline
	return nil
}

// RemovePipeline removes a pipeline from the bridge.
// Returns error if the bridge is running.
func (b *Bridge) RemovePipeline(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot remove pipeline from running bridge")
	}

	delete(b.pipelines, id)
	return nil
}

// GetPipeline returns a pipeline by ID.
func (b *Bridge) GetPipeline(id string) (types.Pipeline, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	pipeline, ok := b.pipelines[id]
	return pipeline, ok
}

// AddRoute adds a route to the bridge.
// Returns error if the bridge is running or if a route with the same ID exists.
func (b *Bridge) AddRoute(route types.Route) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot add route to running bridge")
	}

	id := route.GetID()
	if _, exists := b.routes[id]; exists {
		return fmt.Errorf("route with ID %s already exists", id)
	}

	b.routes[id] = route
	return nil
}

// RemoveRoute removes a route from the bridge.
// Returns error if the bridge is running.
func (b *Bridge) RemoveRoute(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot remove route from running bridge")
	}

	delete(b.routes, id)
	return nil
}

// GetRoute returns a route by ID.
func (b *Bridge) GetRoute(id string) (types.Route, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	route, ok := b.routes[id]
	return route, ok
}

// ListPipelines returns all pipeline IDs.
func (b *Bridge) ListPipelines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	ids := make([]string, 0, len(b.pipelines))
	for id := range b.pipelines {
		ids = append(ids, id)
	}
	return ids
}

// ListRoutes returns all route IDs.
func (b *Bridge) ListRoutes() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	ids := make([]string, 0, len(b.routes))
	for id := range b.routes {
		ids = append(ids, id)
	}
	return ids
}

// Stats returns aggregated statistics for all pipelines.
func (b *Bridge) Stats() map[string]types.PipelineStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	stats := make(map[string]types.PipelineStats)

	for id, pipeline := range b.pipelines {
		if sp, ok := pipeline.(types.StatsProvider); ok {
			stats[id] = sp.Stats()
		}
	}

	for id, route := range b.routes {
		if ri, ok := route.(*RouteImpl); ok {
			stats["route:"+id] = ri.Stats()
		}
	}

	return stats
}

// IsRunning returns true if the bridge is currently running.
func (b *Bridge) IsRunning() bool {
	return b.running.Load()
}

// CreatePipeline creates a new pipeline using the registered factories.
// This is a convenience method that creates a pipeline from configuration.
func (b *Bridge) CreatePipeline(
	ctx context.Context,
	id string,
	sourceConfig types.SourceConfig,
	targetConfig types.TargetConfig,
	middlewareNames []string,
	opts ...PipelineOption,
) (types.Pipeline, error) {
	// Create source
	source, err := b.sourceRegistry.CreateSource(ctx, sourceConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create source: %w", err)
	}

	// Create target
	target, err := b.targetRegistry.CreateTarget(ctx, targetConfig)
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("failed to create target: %w", err)
	}

	// Create middleware chain
	chain, err := b.middlewareRegistry.CreateChain(middlewareNames...)
	if err != nil {
		_ = source.Close()
		_ = target.Close()
		return nil, fmt.Errorf("failed to create middleware chain: %w", err)
	}

	// Create pipeline
	pipeline := NewPipeline(id, types.PipelineModeSimplex, source, target, chain, opts...)

	return pipeline, nil
}

