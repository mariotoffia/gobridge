# Missing Features for Production Readiness

This document outlines what is missing in `bridge/core/bridge.go` and related components to allow users to build and run a bridge instance as standalone or in a cluster.

**Related Documentation:**
- [ARCHITECTURE.md](./ARCHITECTURE.md) - Current architecture

---

## Summary Table

| Category | Feature | Standalone | Cluster | Priority | Status |
|----------|---------|:----------:|:-------:|:--------:|:------:|
| **Initialization** | | | | | |
| | Connection Registry integration | Required | Required | High | ✅ Done |
| | Transport factory auto-registration | Optional | Optional | Medium | Missing |
| | Config-driven pipeline creation | Required | Required | High | ✅ Done |
| | Graceful startup with readiness probe | Optional | Required | Medium | ✅ Done |
| **Runtime** | | | | | |
| | Dynamic pipeline add/remove while running | Optional | Required | High | ✅ Done |
| | ConfigChangeHandler integration | Required | Required | High | ✅ Done |
| | Health check endpoint | Optional | Required | High | ✅ Done |
| | Metrics collection | Optional | Required | High | ✅ Done |
| | Structured logging | Optional | Required | Medium | ✅ Done |
| **Lifecycle** | | | | | |
| | Graceful shutdown with drain | Required | Required | High | ✅ Done |
| | Signal handling (SIGTERM, SIGINT) | Required | Optional | High | ✅ Done |
| | Drain timeout configuration | Required | Required | Medium | ✅ Done |
| | Shutdown hooks | Optional | Optional | Low | ✅ Done |
| **Error Handling** | | | | | |
| | Global error handler | Optional | Optional | Medium | Missing |
| | Panic recovery | Required | Required | High | ✅ Done |
| | Circuit breaker for targets | Optional | Optional | Medium | Missing |
| **Clustering** | | | | | |
| | ClusterConfigurator integration | N/A | Required | High | Missing |
| | Leader election awareness | N/A | Required | High | Missing |
| | Distributed drain coordination | N/A | Required | High | Missing |
| | Shared subscription coordination | N/A | Required | High | Missing |
| | Node discovery | N/A | Required | High | Missing |
| **Configuration** | | | | | |
| | Environment variable support | Required | Required | High | ✅ Done |
| | Config file loading | Required | Optional | High | Missing |
| | Config validation | Required | Required | High | ✅ Done |
| | Secret management integration | Optional | Required | High | Partial |
| **Observability** | | | | | |
| | OpenTelemetry tracing | Optional | Required | Medium | ✅ Done |
| | Prometheus metrics export | Optional | Required | Medium | Partial |
| | Distributed tracing context | Optional | Required | Medium | ✅ Done |
| | Log correlation IDs | Optional | Required | Medium | Partial |
| **Admin** | | | | | |
| | HTTP admin API | Optional | Optional | Low | ✅ Done |
| | Pipeline status endpoint | Optional | Optional | Medium | ✅ Done |
| | Runtime config reload | Optional | Required | Medium | ✅ Done |
| | DLQ management API | Optional | Optional | Low | ✅ Done |
| | Message injection/testing | Optional | Optional | Medium | ✅ Done |
| **Monitoring** | | | | | |
| | Monitor API (metrics/health) | Optional | Required | High | ✅ Done |
| | Prometheus metrics endpoint | Optional | Required | Medium | ✅ Done |
| | OpenTelemetry trace viewer | Optional | Optional | Medium | ✅ Done |
| | Cluster monitoring | N/A | Required | High | ✅ Done |
| | Real-time streaming (WebSocket) | Optional | Optional | Low | ✅ Done |

---

## Detailed Analysis

### 1. Initialization

#### Connection Registry Integration

**Current State:** Bridge has SourceRegistry, TargetRegistry, MiddlewareRegistry but no ConnectionRegistry.

**What's Needed:**
```go
type Bridge struct {
    // ... existing fields ...
    connectionRegistry types.ConnectionRegistry  // ADD THIS
}

func WithConnectionRegistry(registry types.ConnectionRegistry) BridgeOption {
    return func(b *Bridge) {
        b.connectionRegistry = registry
    }
}
```

**Impact:** Without ConnectionRegistry, users cannot create pipelines from shared connections (e.g., MQTT).

---

#### Config-Driven Pipeline Creation

**Current State:** `CreatePipeline` takes individual configs, not a unified `PipelineConfig`.

**What's Needed:**
```go
// Add to Bridge
func (b *Bridge) CreatePipelineFromConfig(ctx context.Context, config types.PipelineConfig) (types.Pipeline, error) {
    // Handle ConnectionID for shared connections
    if connID := config.GetConnectionID(); connID != "" {
        conn, err := b.connectionRegistry.GetConnection(connID)
        // Create from connection's providers
    }
    // Create from factories
}

// Add for Routes
func (b *Bridge) CreateRouteFromConfig(ctx context.Context, config types.RouteConfig) (types.Route, error)
```

---

#### Startup Readiness

**What's Needed:**
```go
type Bridge struct {
    ready atomic.Bool
}

func (b *Bridge) IsReady() bool {
    return b.ready.Load()
}

func (b *Bridge) WaitForReady(ctx context.Context) error {
    // Wait for all pipelines to be ready
}
```

---

### 2. Runtime

#### Dynamic Pipeline Management

**Current State:** Cannot add/remove pipelines while running.

**What's Needed:**
```go
func (b *Bridge) AddPipelineRunning(ctx context.Context, pipeline types.Pipeline) error {
    // Start pipeline immediately
    // Add to pipelines map
}

func (b *Bridge) RemovePipelineRunning(ctx context.Context, id string) error {
    // Drain pipeline
    // Remove from pipelines map
}
```

---

#### ConfigChangeHandler Integration

**Current State:** `handleConfigChange` is a TODO placeholder.

**What's Needed:**
```go
type Bridge struct {
    configHandler types.ConfigChangeHandler
}

func (b *Bridge) handleConfigChange(ctx context.Context, change types.ConfigChange) {
    switch change.Item.GetType() {
    case types.ConfigItemTypeConnection:
        b.configHandler.HandleConnectionChange(ctx, change)
    case types.ConfigItemTypeSource:
        b.configHandler.HandleSourceChange(ctx, change)
    case types.ConfigItemTypeTarget:
        b.configHandler.HandleTargetChange(ctx, change)
    case types.ConfigItemTypePipeline:
        b.handlePipelineChange(ctx, change)
    }
}
```

---

#### Health Checks

**What's Needed:**
```go
type Bridge struct {
    healthCheckers []types.HealthChecker
}

func (b *Bridge) Health(ctx context.Context) *types.HealthCheck {
    // Aggregate health from all pipelines, connections
}

// Interface for external health endpoints
type HealthProvider interface {
    Health(ctx context.Context) *types.HealthCheck
    IsReady() bool
    IsLive() bool
}
```

---

#### Metrics Collection

**What's Needed:**
```go
type Bridge struct {
    metricsCollector types.MetricsCollector
}

func (b *Bridge) Metrics() types.BridgeMetrics {
    return types.BridgeMetrics{
        Pipelines:    b.pipelineMetrics(),
        Connections:  b.connectionMetrics(),
        Retry:        b.retryMetrics(),
    }
}
```

---

### 3. Lifecycle

#### Signal Handling

**What's Needed:**
```go
// Convenience function for standalone operation
func (b *Bridge) Run(ctx context.Context) error {
    ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
    defer cancel()
    
    if err := b.Start(ctx); err != nil {
        return err
    }
    
    <-ctx.Done()
    
    shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), b.shutdownTimeout)
    defer shutdownCancel()
    
    return b.Stop(shutdownCtx)
}
```

---

#### Graceful Drain

**Current State:** `Stop` closes pipelines but doesn't drain.

**What's Needed:**
```go
type Bridge struct {
    drainTimeout time.Duration
}

func (b *Bridge) Stop(ctx context.Context) error {
    // 1. Stop accepting new work
    // 2. Drain all pipelines
    for _, pipeline := range b.pipelines {
        if drainable, ok := pipeline.(types.Drainable); ok {
            drainable.Drain(ctx, types.DrainOptions{
                Timeout:         b.drainTimeout,
                WaitForInFlight: true,
            })
        }
    }
    // 3. Close connections
    // 4. Final cleanup
}
```

---

### 4. Error Handling

#### Panic Recovery

**What's Needed:**
```go
func (b *Bridge) processMessages(ctx context.Context, pipeline types.Pipeline) {
    defer func() {
        if r := recover(); r != nil {
            b.logger.Error("panic in pipeline", "pipeline", pipeline.GetID(), "panic", r)
            // Optionally restart pipeline
        }
    }()
    // Process messages
}
```

---

### 5. Clustering

#### ClusterConfigurator Integration

**What's Needed:**
```go
type Bridge struct {
    clusterConfig types.ClusterConfigurator
}

func WithClusterConfigurator(cc types.ClusterConfigurator) BridgeOption {
    return func(b *Bridge) {
        b.clusterConfig = cc
        b.configSource = cc  // ClusterConfigurator IS a ConfigSource
    }
}

func (b *Bridge) Start(ctx context.Context) error {
    if b.clusterConfig != nil {
        if err := b.clusterConfig.WaitForReady(ctx); err != nil {
            return err
        }
    }
    // Continue with normal startup
}

func (b *Bridge) Stop(ctx context.Context) error {
    if b.clusterConfig != nil {
        if err := b.clusterConfig.RequestDrain(ctx); err != nil {
            return err
        }
    }
    // Continue with normal shutdown
}
```

---

### 6. Configuration

#### Environment Variables

**What's Needed:**
```go
// Helper for common patterns
type BridgeConfig struct {
    ID             string        `env:"BRIDGE_ID" default:"bridge-1"`
    ClusterID      string        `env:"CLUSTER_ID" default:"default"`
    ConfigSource   string        `env:"CONFIG_SOURCE"` // file:// or dynamodb://
    ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" default:"30s"`
    // ...
}

func NewBridgeFromEnv() (*Bridge, error) {
    config := LoadConfigFromEnv()
    return NewBridgeFromConfig(config)
}
```

---

#### Config Validation

**What's Needed:**
```go
func (b *Bridge) Validate() error {
    var errs []error
    
    for id, pipeline := range b.pipelines {
        if err := validatePipeline(pipeline); err != nil {
            errs = append(errs, fmt.Errorf("pipeline %s: %w", id, err))
        }
    }
    
    return errors.Join(errs...)
}
```

---

### 7. Observability

#### OpenTelemetry Integration

**What's Needed:**
```go
type Bridge struct {
    tracer trace.Tracer
    meter  metric.Meter
}

func WithTracer(tracer trace.Tracer) BridgeOption
func WithMeter(meter metric.Meter) BridgeOption
```

---

## Implementation Roadmap

### Phase 1: Standalone Production Ready ✅ COMPLETE

1. ✅ Connection Registry integration - `WithConnectionRegistry()`
2. ✅ Signal handling and graceful shutdown - `Run()` method
3. ✅ Panic recovery - `startWithRecovery()` and handlers
4. ✅ Health checks - `Health()`, `IsReady()`, `IsLive()`, `WaitForReady()`
5. ✅ Basic metrics - `MetricsCollector` interface and `Metrics()`
6. ✅ Config validation - `Validate()` method

### Phase 2: Dynamic Operation ✅ COMPLETE

1. ✅ ConfigChangeHandler integration - `WithConfigHandler()` and `handleConfigChange()`
2. ✅ Dynamic pipeline add/remove - `AddPipelineRunning()`, `RemovePipelineRunning()`
3. ✅ Config-driven creation - `CreatePipelineFromConfig()`, `CreateRouteFromConfig()`
4. Partial: Runtime config reload - via ConfigChangeHandler
5. Missing: DLQ management API

### Phase 3: Clustering (Pending)

1. Missing: ClusterConfigurator integration
2. Missing: Leader election
3. Missing: Distributed drain
4. Missing: Shared subscriptions

### Phase 4: Enterprise Features (Partial)

1. ✅ OpenTelemetry integration - `WithTracer()`, `WithMeter()` interfaces
2. ✅ Admin API - OpenAPI 3.1 spec + implementation
3. ✅ Monitor API - OpenAPI 3.1 spec + implementation
4. Missing: Advanced circuit breakers
5. Missing: Multi-tenancy support

### Phase 5: HTTP APIs ✅ COMPLETE

Both OpenAPI 3.1 specifications AND implementations are complete.

| API | Spec Location | Implementation |
|-----|---------------|----------------|
| Admin API | `apis/http/admin/admin-api.yaml` | `apis/http/server/admin_handlers.go` |
| Monitor API | `apis/http/monitor/monitor-api.yaml` | `apis/http/server/monitor_handlers.go` |

#### Admin API Features (✅ Implemented)
- Bridge lifecycle (start/stop/drain)
- Connection CRUD
- Pipeline/Route management
- DLQ management (view, replay, purge)
- Configuration (reload, validate, diff)
- Message injection for testing
- Diagnostics (logs, errors, debug)

#### Monitor API Features (✅ Implemented)
- Health checks (Kubernetes-compatible)
- Prometheus metrics endpoint
- OpenTelemetry tracing integration
- Instance monitoring
- Cluster monitoring
- Alert management
- WebSocket streaming (complete - uses gorilla/websocket)

#### InjectMiddleware (✅ Implemented)
- `middleware/inject/inject.go` - Inject messages directly into pipelines for testing

---

## Completed Features

### New Types Added

| Type | Location | Description |
|------|----------|-------------|
| `HealthCheck` | `types/health.go` | Health status structure |
| `HealthStatus` | `types/health.go` | Health status enum |
| `HealthProvider` | `types/health.go` | Interface for health reporting |
| `ReadinessProvider` | `types/health.go` | Interface for readiness checks |
| `LivenessProvider` | `types/health.go` | Interface for liveness checks |
| `HealthChecker` | `types/health.go` | Combined health interface |
| `BridgeMetrics` | `types/metrics.go` | Aggregated bridge metrics |
| `PipelineMetrics` | `types/metrics.go` | Pipeline-level metrics |
| `ConnectionMetrics` | `types/metrics.go` | Connection-level metrics |
| `MetricsCollector` | `types/metrics.go` | Interface for metrics collection |
| `MetricsProvider` | `types/metrics.go` | Interface for metrics access |
| `BridgeConfig` | `types/bridge_config.go` | Configuration structure |
| `ValidationError` | `types/bridge_config.go` | Validation error type |
| `Tracer` | `types/observability.go` | Distributed tracing interface |
| `Meter` | `types/observability.go` | Metrics collection interface |

### New Bridge Methods

| Method | Description |
|--------|-------------|
| `Run(ctx)` | Starts bridge and blocks until signal (SIGTERM/SIGINT) |
| `Health(ctx)` | Returns health status |
| `IsReady()` | Returns true when ready to serve traffic |
| `WaitForReady(ctx)` | Blocks until ready |
| `IsLive()` | Returns true if not in fatal state |
| `Metrics(ctx)` | Returns aggregated metrics |
| `Validate()` | Validates configuration |
| `AddPipelineRunning(ctx, p)` | Adds and starts pipeline while running |
| `RemovePipelineRunning(ctx, id)` | Drains and removes pipeline while running |
| `CreatePipelineFromConfig(ctx, config)` | Creates pipeline from PipelineConfig |
| `CreateRouteFromConfig(ctx, config)` | Creates route from RouteConfig |

### New Bridge Options

| Option | Description |
|--------|-------------|
| `WithConnectionRegistry(r)` | Sets connection registry |
| `WithConfigHandler(h)` | Sets config change handler |
| `WithMetricsCollector(c)` | Sets metrics collector |
| `WithShutdownTimeout(d)` | Sets graceful shutdown timeout |
| `WithDrainTimeout(d)` | Sets pipeline drain timeout |
| `WithLogger(l)` | Sets structured logger |
| `WithClusterID(id)` | Sets cluster identifier |
| `WithShutdownHook(fn)` | Adds shutdown hook callback |
| `WithTracer(t)` | Sets distributed tracer |
| `WithMeter(m)` | Sets metrics meter |

### Factory Functions

| Function | Description |
|----------|-------------|
| `NewBridgeFromConfig(config, opts...)` | Creates bridge from BridgeConfig |
| `NewBridgeFromEnv(opts...)` | Creates bridge from environment variables |
| `LoadBridgeConfigFromEnv()` | Loads config from environment |
