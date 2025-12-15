# Middleware Architecture

This document describes the middleware and error handling systems in GoBridge.

**Related Documentation:**
- [ARCHITECTURE.md](./ARCHITECTURE.md) - Main architecture overview
- [ARCHITECTURE-TRANSPORTS.md](./ARCHITECTURE-TRANSPORTS.md) - Transport implementations

## Middleware Chain

Middlewares process messages in a pipeline between Source and Target.

```mermaid
flowchart LR
    Source[Source] --> MW1[Logging] --> MW2[Transform] --> MW3[Filter] --> MW4[Retry] --> Target[Target]
```

### Middleware Interface

```go
type Middleware interface {
    Name() string
    Process(ctx context.Context, msg *Message, next MiddlewareFunc) error
}

type MiddlewareFunc func(ctx context.Context, msg *Message) error
```

### Creating Middleware

```go
// Using MiddlewareAdapter for simple cases
loggingMW := types.NewMiddlewareAdapter("logging",
    func(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
        log.Printf("Processing message: %s", msg.ID)
        err := next(ctx, msg)
        if err != nil {
            log.Printf("Message failed: %v", err)
        }
        return err
    },
)

// Implementing the interface for complex cases
type TransformMiddleware struct {
    transformer Transformer
}

func (m *TransformMiddleware) Name() string { return "transform" }

func (m *TransformMiddleware) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
    transformed, err := m.transformer.Transform(msg.Payload)
    if err != nil {
        return types.ErrInvalidPayload.Wrap(err)
    }
    msg.Payload = transformed
    return next(ctx, msg)
}
```

### Middleware Chain

```go
chain := types.NewMiddlewareChain(
    loggingMiddleware,
    transformMiddleware,
    filterMiddleware,
)

// Process a message
err := chain.Process(ctx, msg, func(ctx context.Context, msg *types.Message) error {
    return target.Send(ctx, *msg)
})
```

---

## Built-in Middleware

### Filter Middleware

Filters messages based on conditions:

```go
// middleware/filter/
filter := filter.New(filter.Config{
    Conditions: []filter.Condition{
        {Field: "topic", Operator: "prefix", Value: "sensors/"},
        {Field: "payload.type", Operator: "eq", Value: "temperature"},
    },
    Mode: filter.ModeAll, // or ModeAny
})
```

### Transform Middleware

Transforms message payloads:

```go
// middleware/transform/
transform := transform.NewJSON(transform.JSONConfig{
    Mappings: []transform.Mapping{
        {Source: "$.data.temp", Target: "$.temperature"},
        {Source: "$.metadata.id", Target: "$.deviceId"},
    },
})
```

### Logging Middleware

Logs message processing:

```go
// bridge/middleware/transport/logging/
correlation := logging.NewCorrelationMiddleware()
publish := logging.NewPublishLogging(logger)
subscribe := logging.NewSubscribeLogging(logger)
```

---

## Retry System

### Architecture

```mermaid
flowchart TB
    subgraph Pipeline [Pipeline Processing]
        Source[Source] --> MW[Middleware Chain] --> Target[Target]
    end
    
    subgraph ErrorHandling [Error Handling]
        MW -->|error| Classify{Recoverable?}
        Classify -->|Yes| RetryManager[RetryManager]
        Classify -->|No| DLQ[DeadLetterQueue]
        RetryManager -->|exhausted| DLQ
        RetryManager -->|retry| MW
    end
```

### RetryManager Interface

```go
type RetryManager interface {
    Enqueue(ctx context.Context, msg Message, reason error) error
    Start(ctx context.Context, handler Subscriber) error
    Stats() RetryStats
    Purge(ctx context.Context) error
}

type RetryPolicy struct {
    MaxAttempts       int
    InitialBackoff    time.Duration
    MaxBackoff        time.Duration
    BackoffMultiplier float64
    Jitter            float64
    RetryableErrors   []ErrorCode
}
```

### Using Retry Manager

```go
// Create retry manager with policy
policy := types.RetryPolicy{
    MaxAttempts:       5,
    InitialBackoff:    time.Second,
    MaxBackoff:        time.Minute,
    BackoffMultiplier: 2.0,
    Jitter:            0.1,
}

retryManager := retry.NewMemoryRetryManager(policy, dlq)

// Use in pipeline
pipeline := core.NewPipeline(id, mode, source, target, chain,
    core.WithRetryManager(retryManager),
    core.WithDeadLetterQueue(dlq),
)
```

### Retry Middleware

```go
// Create retry middleware
retryMW := types.RetryMiddleware("retry", retryManager, policy)

// Add to chain
chain := types.NewMiddlewareChain(
    loggingMW,
    transformMW,
    retryMW, // Should be last before target
)
```

---

## Dead Letter Queue

### DLQ Interface

```go
type DeadLetterQueue interface {
    Send(ctx context.Context, msg Message, reason error) error
    Consume(ctx context.Context) (<-chan *DLQMessage, error)
    Count(ctx context.Context) (int64, error)
    Purge(ctx context.Context) error
    Replay(ctx context.Context, filter DLQFilter) (replayed int64, err error)
}

type DLQMessage struct {
    Message   Message
    Reason    string
    FailedAt  time.Time
    RetryInfo *RetryInfo
    SourceID  string
}
```

### Using DLQ

```go
// Create DLQ
dlq := retry.NewMemoryDLQ()

// Messages automatically sent to DLQ when:
// 1. Retry attempts exhausted
// 2. Permanent error encountered

// Replay messages from DLQ
replayed, err := dlq.Replay(ctx, types.DLQFilter{
    Topic:       "sensors/#",
    Since:       time.Now().Add(-24 * time.Hour),
    MaxMessages: 100,
})
```

---

## Error Classification

### Error Types

```mermaid
flowchart TB
    Error[Error Occurred]
    
    Error --> IsBridge{BridgeError?}
    IsBridge -->|No| Recoverable1[Treat as Recoverable]
    IsBridge -->|Yes| CheckRecoverable{IsRecoverable?}
    
    CheckRecoverable -->|Yes| Retry[Enqueue for Retry]
    CheckRecoverable -->|No| DLQ[Send to DLQ]
    
    Retry --> Exhausted{Exhausted?}
    Exhausted -->|Yes| DLQ
    Exhausted -->|No| Process[Retry Processing]
```

### Recoverable Errors

| Error | Code | Description |
|-------|------|-------------|
| `ErrTimeout` | TIMEOUT | Request timed out |
| `ErrConnectionLost` | CONNECTION_LOST | Connection dropped |
| `ErrUnavailable` | UNAVAILABLE | Service unavailable |
| `ErrThrottled` | THROTTLED | Rate limited |
| `ErrBrokerBusy` | BROKER_BUSY | Target overloaded |

### Permanent Errors

| Error | Code | Description |
|-------|------|-------------|
| `ErrNotAuthorized` | NOT_AUTHORIZED | Auth failed |
| `ErrForbidden` | FORBIDDEN | Permission denied |
| `ErrInvalidPayload` | INVALID_PAYLOAD | Bad message format |
| `ErrPayloadTooLarge` | PAYLOAD_TOO_LARGE | Exceeds size limit |
| `ErrNotFound` | NOT_FOUND | Resource missing |

### Creating Errors

```go
// Wrap underlying error with classification
return types.ErrTimeout.Wrap(err)

// Add context
return types.ErrThrottled.
    With("topic", topic).
    WithRetryAfter(5 * time.Second).
    Wrap(err)

// Check error type
if types.IsBridgeError(err, types.ErrThrottled) {
    // Handle throttling
}

// Check recoverability
if types.IsRecoverableError(err) {
    // Retry
} else {
    // Send to DLQ
}
```

---

## Middleware Registry

The bridge maintains a registry of middleware factories:

```go
type MiddlewareRegistry struct {
    factories map[string]MiddlewareFactory
}

// Register middleware
registry := core.NewMiddlewareRegistry()
registry.Register("logging", func() types.Middleware {
    return logging.NewMiddleware(logger)
})
registry.Register("transform", func() types.Middleware {
    return transform.NewMiddleware(config)
})

// Create chain by names
chain, err := registry.CreateChain("logging", "transform", "filter")
```

---

## Best Practices

1. **Order Matters**: Place retry middleware last before target
2. **Early Filtering**: Filter messages early to reduce processing
3. **Idempotent Operations**: Design middlewares to be idempotent
4. **Error Context**: Add context to errors for debugging
5. **Metrics**: Emit metrics from middleware for monitoring
6. **Backpressure**: Respect channel capacity in long-running operations
