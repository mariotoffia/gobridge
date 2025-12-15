package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Bridge is the main runtime that manages the lifecycle of all pipelines and routes.
// It coordinates sources, targets, and middlewares to create a message routing system.
//
// The Bridge implements:
//   - types.HealthChecker for health/readiness/liveness checks
//   - types.MetricsProvider for metrics access
type Bridge struct {
	// id is the unique identifier of this bridge instance
	id string

	// clusterID is the cluster this bridge belongs to
	clusterID string

	// sourceRegistry manages source factories
	sourceRegistry types.SourceRegistry

	// targetRegistry manages target factories
	targetRegistry types.TargetRegistry

	// middlewareRegistry manages middleware factories
	middlewareRegistry *MiddlewareRegistry

	// connectionRegistry manages connection factories (optional)
	connectionRegistry types.ConnectionRegistry

	// configSource provides configuration (optional)
	configSource types.ConfigSource

	// configHandler processes configuration changes (optional)
	configHandler types.ConfigChangeHandler

	// metricsCollector collects metrics (optional)
	metricsCollector types.MetricsCollector

	// transportRetry is the default transport retry configuration.
	// Used for Connection, Subscribe, and Publish retries.
	// Can be overridden at the Connection level.
	transportRetry types.TransportRetryConfig

	// flowControl is the default flow control configuration.
	// Used for backpressure (MaxInFlight) and default message TTL.
	// Can be overridden at the Pipeline level.
	flowControl types.FlowControlConfig

	// shutdownTimeout is how long to wait for graceful shutdown
	shutdownTimeout time.Duration

	// drainTimeout is how long to wait when draining pipelines
	drainTimeout time.Duration

	// pipelines holds all active pipelines by ID
	pipelines map[string]types.Pipeline

	// routes holds all active routes by ID
	routes map[string]types.Route

	// connections holds active connections by ID
	connections map[string]types.Connection

	// running indicates if the bridge is running
	running atomic.Bool

	// ready indicates if the bridge is ready to serve traffic
	ready atomic.Bool

	// startedAt is when the bridge was started
	startedAt time.Time

	// mu protects pipelines, routes, and connections maps
	mu sync.RWMutex

	// cancel cancels the bridge context
	cancel context.CancelFunc

	// wg tracks running goroutines
	wg sync.WaitGroup

	// readyCh is closed when the bridge is ready
	readyCh chan struct{}

	// shutdownHooks are called during shutdown
	shutdownHooks []func(context.Context) error

	// logger for structured logging (optional)
	logger Logger

	// tracer for distributed tracing (optional)
	tracer types.Tracer

	// meter for metrics collection (optional)
	meter types.Meter
}

// Logger is a simple logging interface.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

// Ensure Bridge implements the health interfaces
var (
	_ types.HealthChecker   = (*Bridge)(nil)
	_ types.MetricsProvider = (*Bridge)(nil)
)

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

// WithConnectionRegistry sets the connection registry.
func WithConnectionRegistry(registry types.ConnectionRegistry) BridgeOption {
	return func(b *Bridge) {
		b.connectionRegistry = registry
	}
}

// WithConfigSource sets the configuration source.
func WithConfigSource(source types.ConfigSource) BridgeOption {
	return func(b *Bridge) {
		b.configSource = source
	}
}

// WithConfigHandler sets the configuration change handler.
func WithConfigHandler(handler types.ConfigChangeHandler) BridgeOption {
	return func(b *Bridge) {
		b.configHandler = handler
	}
}

// WithMetricsCollector sets the metrics collector.
func WithMetricsCollector(collector types.MetricsCollector) BridgeOption {
	return func(b *Bridge) {
		b.metricsCollector = collector
	}
}

// WithTransportRetry sets the default transport retry configuration.
// This configures retries for infrastructure failures (connection, subscribe, publish).
// Can be overridden at the Connection level.
//
// NOTE: This is for TRANSPORT RETRY (infrastructure failures).
// For MESSAGE RETRY (application failures), configure RetryPolicy on the pipeline.
func WithTransportRetry(config types.TransportRetryConfig) BridgeOption {
	return func(b *Bridge) {
		b.transportRetry = config
	}
}

// WithFlowControl sets the default flow control configuration.
// This configures backpressure (MaxInFlight) and default message TTL.
// Can be overridden at the Pipeline level.
func WithFlowControl(config types.FlowControlConfig) BridgeOption {
	return func(b *Bridge) {
		b.flowControl = config
	}
}

// WithShutdownTimeout sets the graceful shutdown timeout.
func WithShutdownTimeout(timeout time.Duration) BridgeOption {
	return func(b *Bridge) {
		b.shutdownTimeout = timeout
	}
}

// WithDrainTimeout sets the drain timeout for pipelines.
func WithDrainTimeout(timeout time.Duration) BridgeOption {
	return func(b *Bridge) {
		b.drainTimeout = timeout
	}
}

// WithLogger sets the logger.
func WithLogger(logger Logger) BridgeOption {
	return func(b *Bridge) {
		b.logger = logger
	}
}

// WithClusterID sets the cluster identifier.
func WithClusterID(clusterID string) BridgeOption {
	return func(b *Bridge) {
		b.clusterID = clusterID
	}
}

// WithShutdownHook adds a shutdown hook that is called during graceful shutdown.
func WithShutdownHook(hook func(context.Context) error) BridgeOption {
	return func(b *Bridge) {
		b.shutdownHooks = append(b.shutdownHooks, hook)
	}
}

// WithTracer sets the tracer for distributed tracing.
func WithTracer(tracer types.Tracer) BridgeOption {
	return func(b *Bridge) {
		b.tracer = tracer
	}
}

// WithMeter sets the meter for metrics collection.
func WithMeter(meter types.Meter) BridgeOption {
	return func(b *Bridge) {
		b.meter = meter
	}
}

// NewBridge creates a new Bridge instance.
func NewBridge(id string, opts ...BridgeOption) *Bridge {
	b := &Bridge{
		id:                 id,
		clusterID:          "default",
		sourceRegistry:     NewSourceRegistry(),
		targetRegistry:     NewTargetRegistry(),
		middlewareRegistry: NewMiddlewareRegistry(),
		metricsCollector:   &types.NoopMetricsCollector{},
		tracer:             &types.NoopTracer{},
		meter:              &types.NoopMeter{},
		transportRetry:     types.DefaultTransportRetryConfig(),
		flowControl:        types.DefaultFlowControlConfig(),
		shutdownTimeout:    30 * time.Second,
		drainTimeout:       30 * time.Second,
		pipelines:          make(map[string]types.Pipeline),
		routes:             make(map[string]types.Route),
		connections:        make(map[string]types.Connection),
		readyCh:            make(chan struct{}),
	}

	for _, opt := range opts {
		opt(b)
	}

	return b
}

// NewBridgeFromConfig creates a new Bridge from a BridgeConfig.
func NewBridgeFromConfig(config types.BridgeConfig, opts ...BridgeOption) (*Bridge, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	allOpts := []BridgeOption{
		WithClusterID(config.ClusterID),
		WithShutdownTimeout(config.ShutdownTimeout),
		WithDrainTimeout(config.DrainTimeout),
		WithTransportRetry(config.TransportRetry),
		WithFlowControl(config.FlowControl),
	}
	allOpts = append(allOpts, opts...)

	return NewBridge(config.ID, allOpts...), nil
}

// NewBridgeFromEnv creates a new Bridge from environment variables.
func NewBridgeFromEnv(opts ...BridgeOption) (*Bridge, error) {
	config := types.LoadBridgeConfigFromEnv()
	return NewBridgeFromConfig(config, opts...)
}

// GetID returns the bridge identifier.
func (b *Bridge) GetID() string {
	return b.id
}

// GetClusterID returns the cluster identifier.
func (b *Bridge) GetClusterID() string {
	return b.clusterID
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

// ConnectionRegistry returns the connection registry.
func (b *Bridge) ConnectionRegistry() types.ConnectionRegistry {
	return b.connectionRegistry
}

// TransportRetry returns the default transport retry configuration.
// This is used by connections that don't have their own retry config.
func (b *Bridge) TransportRetry() types.TransportRetryConfig {
	return b.transportRetry
}

// FlowControl returns the default flow control configuration.
// This is used by pipelines that don't have their own flow control config.
func (b *Bridge) FlowControl() types.FlowControlConfig {
	return b.flowControl
}

// MetricsCollector returns the metrics collector.
func (b *Bridge) MetricsCollector() types.MetricsCollector {
	return b.metricsCollector
}

// Tracer returns the tracer for distributed tracing.
func (b *Bridge) Tracer() types.Tracer {
	return b.tracer
}

// Meter returns the meter for metrics collection.
func (b *Bridge) Meter() types.Meter {
	return b.meter
}

// Start starts the bridge and all registered pipelines/routes.
func (b *Bridge) Start(ctx context.Context) error {
	if !b.running.CompareAndSwap(false, true) {
		return errors.New("bridge already running")
	}

	b.startedAt = time.Now()
	ctx, b.cancel = context.WithCancel(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()

	b.logInfo("starting bridge", "id", b.id, "cluster", b.clusterID)

	// Start all connections first
	for id, conn := range b.connections {
		if err := conn.Start(ctx, nil); err != nil {
			b.stopAllLocked()
			b.running.Store(false)
			return fmt.Errorf("failed to start connection %s: %w", id, err)
		}
	}

	// Start all routes
	for id, route := range b.routes {
		if err := b.startWithRecovery(ctx, "route", id, func() error {
			return route.Start(ctx)
		}); err != nil {
			b.stopAllLocked()
			b.running.Store(false)
			return fmt.Errorf("failed to start route %s: %w", id, err)
		}
	}

	// Start all standalone pipelines (not part of routes)
	for id, pipeline := range b.pipelines {
		if err := b.startWithRecovery(ctx, "pipeline", id, func() error {
			return pipeline.Start(ctx)
		}); err != nil {
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

	// Mark as ready
	b.ready.Store(true)
	close(b.readyCh)

	b.logInfo("bridge started", "pipelines", len(b.pipelines), "routes", len(b.routes))

	return nil
}

// startWithRecovery wraps a start function with panic recovery.
func (b *Bridge) startWithRecovery(ctx context.Context, componentType, id string, start func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic starting %s %s: %v", componentType, id, r)
			b.logError("panic during start", "component", componentType, "id", id, "panic", r)
		}
	}()
	return start()
}

// Run starts the bridge and blocks until shutdown signal is received.
// This is a convenience method for standalone operation with signal handling.
func (b *Bridge) Run(ctx context.Context) error {
	// Set up signal handling
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start the bridge
	if err := b.Start(ctx); err != nil {
		return err
	}

	// Wait for shutdown signal
	<-ctx.Done()
	b.logInfo("shutdown signal received")

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), b.shutdownTimeout)
	defer shutdownCancel()

	return b.Stop(shutdownCtx)
}

// stopAllLocked stops all pipelines, routes, and connections. Caller must hold mu.
func (b *Bridge) stopAllLocked() {
	for _, pipeline := range b.pipelines {
		_ = pipeline.Close()
	}
	for _, route := range b.routes {
		_ = route.Close()
	}
	for _, conn := range b.connections {
		_ = conn.Close()
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
	defer func() {
		if r := recover(); r != nil {
			b.logError("panic handling config change", "panic", r, "type", change.Item.GetType())
		}
	}()

	// If we have a config handler, delegate to it
	if b.configHandler != nil {
		switch change.Item.GetType() {
		case types.ConfigItemTypeConnection:
			if err := b.configHandler.HandleConnectionChange(ctx, change); err != nil {
				b.logError("failed to handle connection change", "error", err)
			}
		case types.ConfigItemTypeSource:
			if err := b.configHandler.HandleSourceChange(ctx, change); err != nil {
				b.logError("failed to handle source change", "error", err)
			}
		case types.ConfigItemTypeTarget:
			if err := b.configHandler.HandleTargetChange(ctx, change); err != nil {
				b.logError("failed to handle target change", "error", err)
			}
		case types.ConfigItemTypePipeline:
			b.handlePipelineChange(ctx, change)
		case types.ConfigItemTypeRoute:
			b.handleRouteChange(ctx, change)
		default:
			b.logWarn("unhandled config change type", "type", change.Item.GetType())
		}
	}
}

// handlePipelineChange handles pipeline configuration changes.
func (b *Bridge) handlePipelineChange(ctx context.Context, change types.ConfigChange) {
	pipelineID := change.Item.GetSortKey()

	switch change.Type {
	case types.ConfigChangeAdd:
		// Pipeline creation from config is handled by CreatePipelineFromConfig
		b.logInfo("pipeline add requested", "id", pipelineID)

	case types.ConfigChangeUpdate:
		// For updates, we may need to restart the pipeline
		b.logInfo("pipeline update requested", "id", pipelineID)

	case types.ConfigChangeDelete:
		if err := b.RemovePipelineRunning(ctx, pipelineID); err != nil {
			b.logError("failed to remove pipeline", "id", pipelineID, "error", err)
		}
	}
}

// handleRouteChange handles route configuration changes.
func (b *Bridge) handleRouteChange(ctx context.Context, change types.ConfigChange) {
	routeID := change.Item.GetSortKey()

	switch change.Type {
	case types.ConfigChangeAdd:
		b.logInfo("route add requested", "id", routeID)

	case types.ConfigChangeUpdate:
		b.logInfo("route update requested", "id", routeID)

	case types.ConfigChangeDelete:
		b.logInfo("route delete requested", "id", routeID)
	}
}

// Stop gracefully stops the bridge and all pipelines/routes.
func (b *Bridge) Stop(ctx context.Context) error {
	if !b.running.CompareAndSwap(true, false) {
		return nil // Already stopped
	}

	b.logInfo("stopping bridge", "id", b.id)
	b.ready.Store(false)

	// Cancel context to signal all goroutines
	if b.cancel != nil {
		b.cancel()
	}

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		b.logWarn("timeout waiting for goroutines during shutdown")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var errs []error

	// Drain pipelines first if supported
	drainCtx, drainCancel := context.WithTimeout(ctx, b.drainTimeout)
	defer drainCancel()

	for id, pipeline := range b.pipelines {
		if drainable, ok := pipeline.(types.Drainable); ok {
			if err := drainable.Drain(drainCtx, types.DrainOptions{
				Timeout:         b.drainTimeout,
				WaitForInFlight: true,
			}); err != nil {
				b.logWarn("failed to drain pipeline", "id", id, "error", err)
			}
		}
	}

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

	// Stop all connections
	for id, conn := range b.connections {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", id, err))
		}
	}

	// Run shutdown hooks
	for i, hook := range b.shutdownHooks {
		if err := hook(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown hook %d: %w", i, err))
		}
	}

	b.logInfo("bridge stopped", "id", b.id)

	return errors.Join(errs...)
}

// Close is an alias for Stop with background context.
func (b *Bridge) Close() error {
	return b.Stop(context.Background())
}

// AddPipeline adds a pipeline to the bridge.
// Returns error if the bridge is running or if a pipeline with the same ID exists.
// For adding pipelines while running, use AddPipelineRunning.
func (b *Bridge) AddPipeline(pipeline types.Pipeline) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot add pipeline to running bridge; use AddPipelineRunning")
	}

	id := pipeline.GetID()
	if _, exists := b.pipelines[id]; exists {
		return fmt.Errorf("pipeline with ID %s already exists", id)
	}

	b.pipelines[id] = pipeline
	return nil
}

// AddPipelineRunning adds and starts a pipeline to a running bridge.
// Returns error if the bridge is not running or if a pipeline with the same ID exists.
func (b *Bridge) AddPipelineRunning(ctx context.Context, pipeline types.Pipeline) error {
	if !b.running.Load() {
		return errors.New("bridge is not running; use AddPipeline")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	id := pipeline.GetID()
	if _, exists := b.pipelines[id]; exists {
		return fmt.Errorf("pipeline with ID %s already exists", id)
	}

	// Start the pipeline immediately
	if err := b.startWithRecovery(ctx, "pipeline", id, func() error {
		return pipeline.Start(ctx)
	}); err != nil {
		return fmt.Errorf("failed to start pipeline: %w", err)
	}

	b.pipelines[id] = pipeline
	b.logInfo("pipeline added while running", "id", id)
	return nil
}

// RemovePipeline removes a pipeline from the bridge.
// Returns error if the bridge is running.
// For removing pipelines while running, use RemovePipelineRunning.
func (b *Bridge) RemovePipeline(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.running.Load() {
		return errors.New("cannot remove pipeline from running bridge; use RemovePipelineRunning")
	}

	delete(b.pipelines, id)
	return nil
}

// RemovePipelineRunning stops and removes a pipeline from a running bridge.
func (b *Bridge) RemovePipelineRunning(ctx context.Context, id string) error {
	b.mu.Lock()
	pipeline, exists := b.pipelines[id]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("pipeline %s not found", id)
	}
	delete(b.pipelines, id)
	b.mu.Unlock()

	// Drain if supported
	if drainable, ok := pipeline.(types.Drainable); ok {
		drainCtx, cancel := context.WithTimeout(ctx, b.drainTimeout)
		defer cancel()
		if err := drainable.Drain(drainCtx, types.DrainOptions{WaitForInFlight: true}); err != nil {
			b.logWarn("failed to drain pipeline during removal", "id", id, "error", err)
		}
	}

	// Close the pipeline
	if err := pipeline.Close(); err != nil {
		return fmt.Errorf("failed to close pipeline %s: %w", id, err)
	}

	b.logInfo("pipeline removed while running", "id", id)
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

// ============================================================================
// Health, Readiness, and Liveness (implements types.HealthChecker)
// ============================================================================

// Health returns the current health status of the bridge.
func (b *Bridge) Health(ctx context.Context) *types.HealthCheck {
	health := types.NewHealthCheck()
	health.Details["bridgeId"] = b.id
	health.Details["clusterId"] = b.clusterID
	health.Details["running"] = b.running.Load()
	health.Details["ready"] = b.ready.Load()

	if !b.startedAt.IsZero() {
		health.Details["uptime"] = time.Since(b.startedAt).String()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Check pipeline health
	for id, pipeline := range b.pipelines {
		pipelineHealth := types.NewHealthCheck()
		if hp, ok := pipeline.(types.HealthProvider); ok {
			pipelineHealth = hp.Health(ctx)
		} else {
			// Basic check - is it running?
			if pi, ok := pipeline.(*PipelineImpl); ok && pi.IsRunning() {
				pipelineHealth.Status = types.HealthStatusHealthy
			} else {
				pipelineHealth.SetDegraded("pipeline not running")
			}
		}
		health.AddComponent("pipeline:"+id, pipelineHealth)
	}

	// Check connection health
	for id, conn := range b.connections {
		connHealth := types.NewHealthCheck()
		if hp, ok := conn.(types.HealthProvider); ok {
			connHealth = hp.Health(ctx)
		} else {
			connHealth.Status = types.HealthStatusHealthy
		}
		health.AddComponent("connection:"+id, connHealth)
	}

	return health
}

// IsReady returns true if the bridge is ready to serve traffic.
func (b *Bridge) IsReady() bool {
	return b.ready.Load()
}

// WaitForReady blocks until the bridge is ready or context is cancelled.
func (b *Bridge) WaitForReady(ctx context.Context) error {
	select {
	case <-b.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsLive returns true if the bridge is alive (not in a fatal state).
func (b *Bridge) IsLive() bool {
	// If running, we're alive. If not started yet, also alive.
	// Only dead if we crashed or are in an unrecoverable state.
	return true
}

// ============================================================================
// Metrics (implements types.MetricsProvider)
// ============================================================================

// Metrics returns the current bridge metrics.
func (b *Bridge) Metrics(ctx context.Context) *types.BridgeMetrics {
	b.mu.RLock()
	defer b.mu.RUnlock()

	metrics := &types.BridgeMetrics{
		BridgeID:    b.id,
		StartedAt:   b.startedAt,
		Pipelines:   make(map[string]types.PipelineMetrics),
		Connections: make(map[string]types.ConnectionMetrics),
	}

	if !b.startedAt.IsZero() {
		metrics.Uptime = time.Since(b.startedAt)
	}

	// Collect pipeline metrics
	for id, pipeline := range b.pipelines {
		pm := types.PipelineMetrics{ID: id}
		if sp, ok := pipeline.(types.StatsProvider); ok {
			pm.Stats = sp.Stats()
		}
		if pi, ok := pipeline.(*PipelineImpl); ok && pi.IsRunning() {
			pm.Status = "running"
		} else {
			pm.Status = "stopped"
		}
		metrics.Pipelines[id] = pm
	}

	// Collect connection metrics
	for id, conn := range b.connections {
		cm := types.ConnectionMetrics{
			ID:            id,
			TransportType: conn.GetTransportType(),
			Status:        "connected",
		}
		metrics.Connections[id] = cm
	}

	return metrics
}

// ============================================================================
// Validation
// ============================================================================

// Validate validates the bridge configuration and state.
func (b *Bridge) Validate() error {
	var errs []error

	if b.id == "" {
		errs = append(errs, &types.ValidationError{Field: "id", Message: "bridge ID is required"})
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Validate pipelines have unique IDs
	pipelineIDs := make(map[string]bool)
	for id := range b.pipelines {
		if pipelineIDs[id] {
			errs = append(errs, &types.ValidationError{Field: "pipelines", Message: fmt.Sprintf("duplicate pipeline ID: %s", id)})
		}
		pipelineIDs[id] = true
	}

	// Validate routes
	routeIDs := make(map[string]bool)
	for id := range b.routes {
		if routeIDs[id] {
			errs = append(errs, &types.ValidationError{Field: "routes", Message: fmt.Sprintf("duplicate route ID: %s", id)})
		}
		routeIDs[id] = true
	}

	if len(errs) > 0 {
		return &types.ConfigValidationError{Errors: errs}
	}
	return nil
}

// ============================================================================
// Pipeline Creation
// ============================================================================

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

	// Create pipeline with flow control from bridge defaults
	allOpts := []PipelineOption{
		PipelineWithFlowControl(b.flowControl),
	}
	allOpts = append(allOpts, opts...)

	pipeline := NewPipeline(id, types.PipelineModeSimplex, source, target, chain, allOpts...)

	return pipeline, nil
}

// CreatePipelineFromConfig creates a pipeline from a PipelineConfig.
// If the config specifies a ConnectionID, the source and target are created
// from the shared connection's providers.
func (b *Bridge) CreatePipelineFromConfig(ctx context.Context, config types.PipelineConfig) (types.Pipeline, error) {
	var source types.Source
	var target types.Target
	var err error

	// Check if using a shared connection
	if connID := config.GetConnectionID(); connID != "" {
		if b.connectionRegistry == nil {
			return nil, errors.New("connection registry not configured")
		}

		conn, err := b.connectionRegistry.GetConnection(connID)
		if err != nil {
			return nil, fmt.Errorf("connection %s not found: %w", connID, err)
		}

		// Create source from connection's provider
		if sp := conn.SourceProvider(); sp != nil {
			source, err = sp.CreateSource(ctx, config.GetSourceConfig())
			if err != nil {
				return nil, fmt.Errorf("failed to create source from connection: %w", err)
			}
		}

		// Create target from connection's provider
		if tp := conn.TargetProvider(); tp != nil {
			target, err = tp.CreateTarget(ctx, config.GetTargetConfig())
			if err != nil {
				if source != nil {
					_ = source.Close()
				}
				return nil, fmt.Errorf("failed to create target from connection: %w", err)
			}
		}
	}

	// Fall back to registries if not created from connection
	if source == nil {
		source, err = b.sourceRegistry.CreateSource(ctx, config.GetSourceConfig())
		if err != nil {
			return nil, fmt.Errorf("failed to create source: %w", err)
		}
	}

	if target == nil {
		target, err = b.targetRegistry.CreateTarget(ctx, config.GetTargetConfig())
		if err != nil {
			_ = source.Close()
			return nil, fmt.Errorf("failed to create target: %w", err)
		}
	}

	// Create middleware chain
	chain, err := b.middlewareRegistry.CreateChain(config.GetMiddlewareNames()...)
	if err != nil {
		_ = source.Close()
		_ = target.Close()
		return nil, fmt.Errorf("failed to create middleware chain: %w", err)
	}

	// Merge flow control config
	flowControl := b.flowControl
	if fc := config.GetFlowControl(); fc != nil {
		flowControl = flowControl.Merge(*fc)
	}

	// Create pipeline
	pipeline := NewPipeline(
		config.GetID(),
		config.GetMode(),
		source,
		target,
		chain,
		PipelineWithFlowControl(flowControl),
	)

	return pipeline, nil
}

// CreateRouteFromConfig creates a route from a RouteConfig.
func (b *Bridge) CreateRouteFromConfig(ctx context.Context, config types.RouteConfig) (types.Route, error) {
	pipelineConfigs := config.GetPipelineConfigs()
	pipelines := make([]types.Pipeline, 0, len(pipelineConfigs))

	for _, pc := range pipelineConfigs {
		pipeline, err := b.CreatePipelineFromConfig(ctx, pc)
		if err != nil {
			// Close already created pipelines
			for _, p := range pipelines {
				_ = p.Close()
			}
			return nil, fmt.Errorf("failed to create pipeline %s: %w", pc.GetID(), err)
		}
		pipelines = append(pipelines, pipeline)
	}

	return NewRoute(config.GetID(), pipelines...), nil
}

// ============================================================================
// Connection Management
// ============================================================================

// AddConnection adds a connection to the bridge.
func (b *Bridge) AddConnection(conn types.Connection) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := conn.GetID()
	if _, exists := b.connections[id]; exists {
		return fmt.Errorf("connection with ID %s already exists", id)
	}

	b.connections[id] = conn
	return nil
}

// GetConnection returns a connection by ID.
func (b *Bridge) GetConnection(id string) (types.Connection, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	conn, ok := b.connections[id]
	return conn, ok
}

// ListConnections returns all connection IDs.
func (b *Bridge) ListConnections() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	ids := make([]string, 0, len(b.connections))
	for id := range b.connections {
		ids = append(ids, id)
	}
	return ids
}

// ============================================================================
// Logging Helpers
// ============================================================================

func (b *Bridge) logDebug(msg string, keysAndValues ...any) {
	if b.logger != nil {
		b.logger.Debug(msg, keysAndValues...)
	}
}

func (b *Bridge) logInfo(msg string, keysAndValues ...any) {
	if b.logger != nil {
		b.logger.Info(msg, keysAndValues...)
	}
}

func (b *Bridge) logWarn(msg string, keysAndValues ...any) {
	if b.logger != nil {
		b.logger.Warn(msg, keysAndValues...)
	}
}

func (b *Bridge) logError(msg string, keysAndValues ...any) {
	if b.logger != nil {
		b.logger.Error(msg, keysAndValues...)
	}
}
