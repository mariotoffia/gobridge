# Plugin Guide

This guide explains how to extend gobridge with custom transport adapters, store backends, credential repositories, observability exporters, and message processors.

All extension points follow the hexagonal architecture: implement a port interface from `ports/`, register it with the `bridge.Builder`, and gobridge handles the rest.

## Transport Adapters

Transport adapters connect gobridge to messaging systems. A transport provides message ingress (Receiver), egress (Sender), and optionally a stateful connection (Session).

### Port Interfaces

From `ports/transport.go`:

```go
type Delivery interface {
    Envelope() *domain.Envelope
    Ack(ctx context.Context) error
    Retry(ctx context.Context, after time.Duration, reason error) error
    Extend(ctx context.Context, until time.Time) error
}

type Receiver interface {
    Run(ctx context.Context, emit func(context.Context, Delivery) error) error
}

type Sender interface {
    Send(ctx context.Context, env *domain.Envelope) error
}

type BatchSender interface {
    Sender
    SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error)
}

type Session interface {
    Start(ctx context.Context) error
    Reconcile(ctx context.Context, plan domain.SessionPlan) error
    Health(ctx context.Context) SessionHealth
    Events() <-chan SessionEvent
    Close(ctx context.Context) error
}
```

### Transport Factory (ports-first)

To integrate with the builder, implement `ports.TransportFactory` (from `ports/factories.go`):

```go
type TransportFactory interface {
    NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error)
    NewReceiver(ctx context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error)
    NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error)
    Capabilities() []ports.Capability
}
```

The bridge converts the declarative `config.*Def` shapes into generic
`ports.*Spec` values before invoking the factory. The plugin only sees
ports types, so a transport adapter never needs to import `bridge` or
`config` — its only inner-ring dependencies are `ports` (and `domain`,
`logging` as needed).

For stateless transports, `NewSession` should return `(nil, nil)`.

Optional companion interfaces (also in `ports`):

- `ports.VisibilityTimeoutProvider` — declares the source visibility
  timeout used by the runtime validator (e.g. SQS).
- `ports.HTTPMountable` — declares an `http.Handler` for transports
  that expose HTTP endpoints (e.g. the HTTP source/SSE sink).

### Registration

```go
builder.RegisterTransport("mytransport", myTransportFactory)
```

The `"mytransport"` name must match the `transport` field in config YAML:

```yaml
receivers:
  - id: my-receiver
    transport: mytransport
    options:
      endpoint: "..."
```

### Implementation Pattern

**Typical file layout:**

```
adapters/mycloud/transport/myqueue/
├── doc.go              # Package documentation
├── go.mod              # Separate module
├── config.go           # ReceiverConfig, SenderConfig, option parsing
├── errors.go           # Map SDK errors to domain.BridgeError
├── receiver.go         # ports.Receiver implementation
├── sender.go           # ports.Sender implementation
├── delivery.go         # ports.Delivery implementation
├── headers.go          # Transport-specific header mapping
├── factory.go          # Transport factory (ports.TransportFactory)
├── receiver_test.go    # Unit tests
└── sender_test.go
```

**Key implementation concerns:**

1. **Delivery mapping**: Map transport-native ack/nack/extend to `ports.Delivery`. For example, SQS maps `Ack` to `DeleteMessage`, `Retry` to `ChangeMessageVisibility`, `Extend` to visibility extension.

2. **Error mapping**: Create an `errors.go` that maps SDK error codes to `domain.BridgeError` with correct classification (Transient vs Permanent vs Rejected).

3. **Header mapping**: Map transport-native message properties to `domain.Envelope.Headers` and vice versa. Strip `x-bridge.*` reserved headers at ingress.

4. **Config parsing**: Parse `Options map[string]any` from `ports.*Spec`
   values into typed structs that live INSIDE the plugin package.
   Provide `ReceiverConfigFromOptions`, `SenderConfigFromOptions`, and
   `SessionOptionsFromMap` helpers (or equivalent). Plugin-specific
   shapes MUST NOT be added to the core `config` package — keeping core
   `config` generic is what lets new plugins ship without forking
   gobridge.

5. **Capabilities**: Return appropriate `ports.Capability` values:
   - `CapStatefulSession` -- transport uses sessions (e.g. MQTT)
   - `CapVisibilityExtension` -- supports deadline extension (e.g. SQS)
   - `CapSourceRedelivery` -- transport redelivers on nack (e.g. SQS)
   - `CapDelayedSend` -- supports delayed delivery
   - `CapSharedConsumer` -- multiple consumers share messages
   - `CapExclusiveIdentity` -- session owns a unique client identity

**Compile-time interface checks:**

```go
var _ ports.Receiver = (*Receiver)(nil)
var _ ports.Sender = (*Sender)(nil)
var _ ports.TransportFactory = (*Factory)(nil)
```

### Reference Implementations

- **Stateful (MQTT)**: `adapters/mqtt/transport/paho/` -- full Session
  with Reconcile; `paho.Factory` directly satisfies `ports.TransportFactory`.
- **Stateless (SQS)**: `adapters/aws/transport/sqs/` -- composes
  `ReceiverFactory` and `SenderFactory` into a unified `Factory` that
  also satisfies `ports.VisibilityTimeoutProvider`.
- **Stateless (ASB)**: `adapters/azure/transport/servicebus/` -- auto-
  extend message lock, batch send with size-limited batches.

## Store Adapters

Store adapters provide persistence for leases, outbox records, and DLQ entries.

### Port Interfaces

From `ports/stores.go`:

```go
type LeaseStore interface {
    Acquire(ctx context.Context, leaseID string, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error)
    Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error)
    Release(ctx context.Context, leaseID string, token domain.LeaseToken) error
    Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error)
}

type OutboxStore interface {
    Persist(ctx context.Context, records []domain.OutboxRecord) error
    Claim(ctx context.Context, partitionKey, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error)
    Complete(ctx context.Context, recordIDs []string, token domain.LeaseToken) error
    Expire(ctx context.Context, before time.Time) (int, error)
    QueryPending(ctx context.Context, partitionKey string, limit int) ([]domain.OutboxRecord, error)
}

type DLQStore interface {
    Write(ctx context.Context, entry domain.DLQEntry) error
    List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error)
    Replay(ctx context.Context, entryIDs []string) error
    Purge(ctx context.Context, before time.Time) (int, error)
}
```

### Store Factory (ports-first)

Implement `ports.StoreFactory` (from `ports/stores.go`):

```go
type StoreFactory interface {
    NewLeaseStore(ctx context.Context, spec ports.StoreSpec) (ports.LeaseStore, error)
    NewOutboxStore(ctx context.Context, spec ports.StoreSpec) (ports.OutboxStore, error)
    NewDLQStore(ctx context.Context, spec ports.StoreSpec) (ports.DLQStore, error)
}
```

`ports.StoreSpec` carries the generic `{ Type, Options }` produced by
the bridge from `config.StoreConfig`. Like transport plugins, store
plugins read their typed shape from `spec.Options` — they never import
the core `config` package.

Optional companion interface:

- `ports.DistributedStoreFactory.IsDistributed() bool` — returns true
  when the store provides cross-process coordination. Required for
  clustered deployments.

Return `(nil, nil)` for store types the factory does not support.

### Registration

```go
builder.RegisterStoreFactory("mybackend", myStoreFactory)
```

Config:

```yaml
stores:
  lease:
    type: mybackend
    options:
      connection_string: "..."
```

### Conformance Testing

Use the built-in conformance test suites in `ports/storetest/`:

```go
func TestMyStore(t *testing.T) {
    store := mybackend.NewStore(/* ... */)
    storetest.RunDLQStoreTests(t, store)
    storetest.RunOutboxStoreTests(t, store)
    storetest.RunLeaseStoreTests(t, store, nil)
}
```

These suites verify all required behaviors (idempotency, filtering, fencing, etc.).

### Reference Implementations

- **Memory**: `adapters/native/store/memory*/` -- sync.Mutex + maps, good for tests
- **SQLite**: `adapters/native/store/sqlite*/` -- WAL mode, modernc.org/sqlite, JSON marshaling
- **DynamoDB**: `adapters/aws/store/dynamodb*/` -- conditional writes, GSIs, TTL compaction

## Credential Adapters

Credential adapters resolve secrets by URI scheme.

### Port Interfaces

From `ports/credentials.go`:

```go
type CredentialRepository interface {
    Scheme() string
    Namespace() string
    Get(ctx context.Context, uri string) (*domain.CredentialSet, error)
}

type CredentialAdmin interface {
    CredentialRepository
    Create(ctx context.Context, uri string, creds *domain.CredentialSet) error
    Update(ctx context.Context, uri string, creds *domain.CredentialSet, version int64) error
    Delete(ctx context.Context, uri string, version int64) error
    List(ctx context.Context, prefix string) ([]string, error)
}
```

### Registration

Register on the `CredentialResolver`:

```go
resolver := runtime.NewCredentialResolver()
resolver.Register(myRepo)
builder := bridge.NewBuilder(cfg, bridge.WithCredentialStore(resolver))
```

The resolver dispatches by URI scheme (`file://`, `pms://`, `vault://`) with longest-prefix namespace matching.

### Domain Types

`domain.CredentialSet` contains optional `*PasswordCredential` and `*TLSMaterial`. Credential values must never appear in logs.

### Reference Implementations

- **File**: `adapters/native/credentials/file/` -- scheme `"file"`, filesystem-based, supports CredentialAdmin
- **SSM**: `adapters/aws/credentials/ssm/` -- scheme `"pms"`, AWS Parameter Store

### Runtime Rotation

Transport sessions (or receivers/senders) that want to accept rotated
credentials on a live connection implement the
`bridge.CredentialAware` capability interface:

```go
type CredentialAware interface {
    ApplyCredentials(ctx context.Context, creds *domain.CredentialSet) error
}
```

The `bridge.CredentialRefresher` discovers participating transports
via a silent type assertion -- non-aware transports (HTTP, stateless
adapters) coexist cleanly in the same bridge.

`ApplyCredentials` receives the full `*CredentialSet` (password and
TLS material together); the implementation dispatches on what
changed and triggers the appropriate rebuild (reconnect for
stateful transports, client swap for stateless ones). See
[`docs/credentials-rotation.md`](docs/credentials-rotation.md) for
the full contract, per-transport behaviour matrix, and worked
examples of adding a new rotatable capability or writing a new
transport that participates in rotation.

## Observability Adapters

### Metrics

Implement `ports.MetricsExporter`:

```go
type MetricsExporter interface {
    Counter(name string, value int64, tags ...domain.Tag)
    Gauge(name string, value float64, tags ...domain.Tag)
    Histogram(name string, value float64, tags ...domain.Tag)
    Timer(name string, duration time.Duration, tags ...domain.Tag)
    Flush(ctx context.Context) error
    Close(ctx context.Context) error
}
```

Pass to runtime: `runtime.WithMetrics(exporter)`.

Reference: `adapters/otel/metrics/` (OTLP), `adapters/aws/metrics/cloudwatch/`.

### Tracing

Implement `ports.Tracer`:

```go
type Tracer interface {
    StartSpan(ctx context.Context, name string, attrs ...domain.Tag) (context.Context, Span)
}

type Span interface {
    End()
    SetError(err error)
    AddEvent(name string, attrs ...domain.Tag)
    SetAttributes(attrs ...domain.Tag)
}
```

Pass to runtime: `runtime.WithTracer(tracer)`.

Reference: `adapters/otel/tracing/`.

## Processors

Processors form an onion-model middleware chain around message delivery.

### Port Interface

From `ports/processor.go`:

```go
type ProcessorFunc func(ctx context.Context, env *domain.Envelope) error

type Processor interface {
    Name() string
    Process(ctx context.Context, env *domain.Envelope, next ProcessorFunc) error
}
```

A processor can:
- **Pass through**: call `next(ctx, env)` to continue the chain
- **Modify**: mutate `env` before calling `next`
- **Short-circuit**: return an error without calling `next`
- **Filter**: return `domain.ErrMessageFiltered` to silently ack without DLQ

### Registration

```go
builder.RegisterProcessor("myproc", myProcessor)
```

Reference in route config:

```yaml
routes:
  - id: my-route
    receiver_id: mqtt-in
    processors: [myproc, transform]
    bindings: [to-sqs]
```

### Implementation Example

```go
package myprocessor

import (
    "context"
    "github.com/mariotoffia/gobridge/domain"
    "github.com/mariotoffia/gobridge/ports"
)

type Processor struct {
    name string
}

var _ ports.Processor = (*Processor)(nil)

func New(name string) *Processor {
    return &Processor{name: name}
}

func (p *Processor) Name() string { return p.name }

func (p *Processor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
    // Pre-processing: enrich headers
    env.Headers = domain.SetHeader(env.Headers, "x-enriched", "true")

    // Call next processor in chain
    if err := next(ctx, env); err != nil {
        return err
    }

    // Post-processing (after downstream completes)
    return nil
}
```

### Reference Implementations

- **filter**: `processors/filter/` -- condition evaluation with operators (eq, ne, contains, regex, gt, lt, exists, in), actions (pass, drop, route)
- **transform**: `processors/transform/` -- JSON field mapping with JSONPath, type coercion
- **circuitbreaker**: `processors/circuitbreaker/` -- per-key state machine, returns `domain.ErrUnavailable.WithRetryAfter()`
- **tenant**: `processors/tenant/` -- header-based tenant extraction, validation, usage tracking

## Module Conventions

1. **Separate go.mod**: Every adapter and processor is its own Go module.
2. **Add to go.work**: `go work use ./adapters/mycloud/...` then `go work sync`.
3. **doc.go**: Every package needs a `doc.go` explaining its purpose.
4. **Compile-time checks**: `var _ ports.X = (*T)(nil)` for all implemented interfaces.
5. **Error mapping**: Transport adapters must have an `errors.go` mapping SDK errors to `domain.BridgeError`.
6. **Config helpers**: Provide `ReceiverConfigFromOptions`, `SenderConfigFromOptions`,
   `SessionOptionsFromMap`, etc. — typed plugin shapes live inside the
   plugin package, never inside core `config`.
7. **Hexagonal direction**: Adapters depend on `ports`, `domain`, and
   their own SDK only. Adapters MUST NOT import `bridge`, `config`,
   `runtime`, or other unrelated adapters. The architecture lint
   (`make lint-arch`) enforces this; cross-adapter imports fail CI.
8. **Config-source adapters are special**: packages under
   `adapters/*/config/*` are the single category allowed to import
   `config` (they exist to load `*config.BridgeConfig`).
