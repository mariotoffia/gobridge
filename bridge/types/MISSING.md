# Missing Features for Production Readiness

This document outlines what is missing in `bridge/core/bridge.go` and related components to allow users to build and run a bridge instance as standalone or in a cluster.

**Related Documentation:**
- [ARCHITECTURE.md](./ARCHITECTURE.md) - Current architecture

---

## Summary Table

| Category | Feature | Standalone | Cluster | Priority | Status |
|----------|---------|:----------:|:-------:|:--------:|:------:|
| **Initialization** | | | | | |
| | Connection Registry integration | Required | Required | High | Missing |
| | Transport factory auto-registration | Optional | Optional | Medium | Missing |
| | Config-driven pipeline creation | Required | Required | High | Partial |
| | Graceful startup with readiness probe | Optional | Required | Medium | Missing |
| **Runtime** | | | | | |
| | Dynamic pipeline add/remove while running | Optional | Required | High | Missing |
| | ConfigChangeHandler integration | Required | Required | High | Partial |
| | Health check endpoint | Optional | Required | High | Missing |
| | Metrics collection | Optional | Required | High | Missing |
| | Structured logging | Optional | Required | Medium | Partial |
| **Lifecycle** | | | | | |
| | Graceful shutdown with drain | Required | Required | High | Partial |
| | Signal handling (SIGTERM, SIGINT) | Required | Optional | High | Missing |
| | Drain timeout configuration | Required | Required | Medium | Missing |
| | Shutdown hooks | Optional | Optional | Low | Missing |
| **Error Handling** | | | | | |
| | Global error handler | Optional | Optional | Medium | Missing |
| | Panic recovery | Required | Required | High | Missing |
| | Circuit breaker for targets | Optional | Optional | Medium | Missing |
| **Clustering** | | | | | |
| | ClusterConfigurator integration | N/A | Required | High | Missing |
| | Leader election awareness | N/A | Required | High | Missing |
| | Distributed drain coordination | N/A | Required | High | Missing |
| | Shared subscription coordination | N/A | Required | High | Missing |
| | Node discovery | N/A | Required | High | Missing |
| **Configuration** | | | | | |
| | Environment variable support | Required | Required | High | Missing |
| | Config file loading | Required | Optional | High | Missing |
| | Config validation | Required | Required | High | Missing |
| | Secret management integration | Optional | Required | High | Partial |
| **Observability** | | | | | |
| | OpenTelemetry tracing | Optional | Required | Medium | Missing |
| | Prometheus metrics export | Optional | Required | Medium | Missing |
| | Distributed tracing context | Optional | Required | Medium | Missing |
| | Log correlation IDs | Optional | Required | Medium | Partial |
| **Admin** | | | | | |
| | HTTP admin API | Optional | Optional | Low | Missing |
| | Pipeline status endpoint | Optional | Optional | Medium | Missing |
| | Runtime config reload | Optional | Required | Medium | Missing |
| | DLQ management API | Optional | Optional | Low | Missing |

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

### Phase 1: Standalone Production Ready

1. Connection Registry integration
2. Signal handling and graceful shutdown
3. Panic recovery
4. Health checks
5. Basic metrics
6. Config validation

### Phase 2: Dynamic Operation

1. ConfigChangeHandler integration
2. Dynamic pipeline add/remove
3. Runtime config reload
4. DLQ management

### Phase 3: Clustering

1. ClusterConfigurator integration
2. Leader election
3. Distributed drain
4. Shared subscriptions

### Phase 4: Enterprise Features

1. Full OpenTelemetry integration
2. Admin API
3. Advanced circuit breakers
4. Multi-tenancy support
