# Credential and observability adapters

## Credential Adapters

Credential adapters resolve secrets by URI scheme.

### Port Interfaces

From `ports/credentials.go`:

```go
type CredentialRepository interface {
    Scheme() string
    Namespace() string
    Get(ctx context.Context, uri string) (*connectivity.CredentialSet, error)
}

type CredentialAdmin interface {
    CredentialRepository
    Create(ctx context.Context, uri string, creds *connectivity.CredentialSet) error
    Update(ctx context.Context, uri string, creds *connectivity.CredentialSet, version int64) error
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

`connectivity.CredentialSet` contains optional `*PasswordCredential` and `*TLSMaterial`. Credential values must never appear in logs.

### Reference Implementations

- **File**: `adapters/native/credentials/file/` -- scheme `"file"`, filesystem-based, supports CredentialAdmin
- **SSM**: `adapters/aws/credentials/ssm/` -- scheme `"pms"`, AWS Parameter Store

### Runtime Rotation

Transport sessions (or receivers/senders) that want to accept rotated
credentials on a live connection implement the
`bridge.CredentialAware` capability interface:

```go
type CredentialAware interface {
    ApplyCredentials(ctx context.Context, creds *connectivity.CredentialSet) error
}
```

The `bridge.CredentialRefresher` discovers participating transports
via a silent type assertion -- non-aware transports (HTTP, stateless
adapters) coexist cleanly in the same bridge.

`ApplyCredentials` receives the full `*CredentialSet` (password and
TLS material together); the implementation dispatches on what
changed and triggers the appropriate rebuild (reconnect for
stateful transports, client swap for stateless ones). See
[`docs/credentials-rotation.md`](../credentials-rotation.md) for
the full contract, per-transport behaviour matrix, and worked
examples of adding a new rotatable capability or writing a new
transport that participates in rotation.

A transport that authenticates on a live connection may call
`CredentialRefresher.NotifyAuthFailure(uri, err)` when the broker reports
`NOT_AUTHORIZED`, forcing an immediate credential re-resolve instead of waiting
for the poll interval (rate-limited per URI). Stock transports do this
automatically: implement the optional `bridge.AuthFailureReporter` capability
(`SetAuthFailureCallback(func(err error))`) and the refresher injects a
URI-bound callback at `Watch` time — the amqp10/amqp091/mqtt sessions report at
reconnect, and the SQS/Service Bus sender+receiver report on the live send and
receive paths. HTTP has no runtime-rotatable session, so it wires nothing.
Resolve and rotation observability is built in and not your plugin's job: the
resolver emits
`CredentialResolveFailure` and `CredentialStaleServed`, the refresher emits
`CredentialRotationApplied`, and the poll wrapper emits
`CredentialRefreshFailures`.

## Observability Adapters

### Metrics

Implement `ports.MetricsExporter`:

```go
type MetricsExporter interface {
    Counter(name string, value int64, tags ...shared.Tag)
    Gauge(name string, value float64, tags ...shared.Tag)
    Histogram(name string, value float64, tags ...shared.Tag)
    Timer(name string, duration time.Duration, tags ...shared.Tag)
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
    StartSpan(ctx context.Context, name string, attrs ...shared.Tag) (context.Context, Span)
}

type Span interface {
    End()
    SetError(err error)
    AddEvent(name string, attrs ...shared.Tag)
    SetAttributes(attrs ...shared.Tag)
}
```

Pass to runtime: `runtime.WithTracer(tracer)`.

Reference: `adapters/otel/tracing/`.
