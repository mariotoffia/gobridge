# Transport adapters

Transport adapters connect gobridge to messaging systems. A transport provides message ingress (Receiver), egress (Sender), and optionally a stateful connection (Session).

### Port Interfaces

From `ports/transport.go`:

```go
type Delivery interface {
    Envelope() *messaging.Envelope
    Ack(ctx context.Context) error
    Retry(ctx context.Context, after time.Duration, reason error) error
    Extend(ctx context.Context, until time.Time) error
}

type Receiver interface {
    Run(ctx context.Context, emit func(context.Context, Delivery) error) error
}

// OutboundMessage carries an envelope together with the per-dispatch
// transport destination resolved by the runtime (DispatchPlan.Address).
// Senders MUST publish to OutboundMessage.Address — they MUST NOT read a
// destination out of OutboundMessage.Envelope.Subject (which is the
// logical event subject and is not a transport destination).
type OutboundMessage struct {
    Envelope *messaging.Envelope
    Address  string
}

type Sender interface {
    Send(ctx context.Context, msg OutboundMessage) error
}

type BatchResult struct {
    Index int   // index into the input slice
    Err   error // nil on success
}

type BatchSender interface {
    Sender
    SendBatch(ctx context.Context, msgs []OutboundMessage) ([]BatchResult, error)
}

type Session interface {
    Start(ctx context.Context) error
    Reconcile(ctx context.Context, plan connectivity.SessionPlan) error
    Health(ctx context.Context) SessionHealth
    Events() <-chan SessionEvent
    Close(ctx context.Context) error
}
```

### Transport Factory (ports-first)

To integrate with the builder, implement `ports.TransportFactory` (from
`ports/factories.go`). Each spec carries a typed `ports.PluginConfig`
the adapter has registered (see [Typed Plugin Config](../../PLUGIN.md#typed-plugin-config)
below — this is the single source of truth for adapter-specific
options, and it replaces the old `Options map[string]any` decoding):

```go
type TransportFactory interface {
    NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error)
    NewReceiver(ctx context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error)
    NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error)
    Capabilities() []ports.Capability
}
```

The bridge converts the declarative `config.*Def` shapes into
`ports.*Spec` values, with the adapter's typed `PluginConfig`
already attached on `Spec.Config`, before invoking the factory. The
plugin only sees ports types, so a transport adapter never needs to
import `bridge` or `config` — its only inner-ring dependencies are
`ports` (and `domain`, `logging` as needed).

For stateless transports, `NewSession` should return `(nil, nil)`.

Optional companion interfaces (also in `ports`):

- `ports.VisibilityTimeoutProvider` — declares the source visibility
  timeout used by the runtime validator (e.g. SQS).
- `ports.IngressMemoryConfig` — lets a typed session config validate a
  transport-owned ingress byte bound against the route's effective concurrency.
  The bridge calls it after dedicated-session cardinality checks and before
  opening stores or transports.
- `ports.IngressMemoryProfileConfig` — extends that contract for deployment
  profiles that assign a per-session byte budget and derive safe transport
  concurrency. Implementations must preserve safe explicit values and reject
  unsafe explicit values rather than silently clamping them.

Transports that expose HTTP endpoints (e.g. the HTTP source / SSE
sink) deliberately do not have a port-level abstraction: HTTP handlers
are inherently HTTP, so the composition root wires them via the
adapter's concrete type rather than through `ports/` (keeping
`net/http` out of the inner ring).

### Registration

```go
builder.RegisterTransportFactory("mytransport", myTransportFactory)
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
├── errors.go           # Map SDK errors to shared.BridgeError
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

2. **Error mapping**: Create an `errors.go` that maps SDK error codes to `shared.BridgeError` with correct classification (Transient vs Permanent vs Rejected).

3. **Header mapping**: Map transport-native message properties to `messaging.Envelope.Headers` and vice versa. Strip `x-bridge.*` reserved headers at ingress.

4. **Typed config**: Export a concrete `Config` struct satisfying
   `ports.PluginConfig` and register its decoder via an exported
   `Register(reg *ports.Registry) error` in `register.go` (the
   composition root calls it explicitly; no `init()`, no process-wide
   registry). The adapter receives its already-decoded typed config via
   `Spec.Config` — it never decodes `map[string]any`, and plugin shapes
   never enter the core `config` package. See
   [Typed Plugin Config](../../PLUGIN.md#typed-plugin-config) below.

5. **Capabilities**: Return appropriate `ports.Capability` values:
   - `CapStatefulSession` -- transport uses sessions (e.g. MQTT)
   - `CapVisibilityExtension` -- supports deadline extension (e.g. SQS)
   - `CapSourceRedelivery` -- transport redelivers on nack (e.g. SQS)
   - `CapDelayedSend` -- supports delayed delivery
   - `CapSharedConsumer` -- broker load-balances one subscription across a consumer group (e.g. MQTT `$share`)
   - `CapExclusiveIdentity` -- session owns a unique client identity (lease-based single holder); **must be single-use** -- see the lifecycle note below
   - `CapDedicatedIngressSession` -- the session is one ingress dispatch/settlement failure domain and permits at most one logical receiver consumed by at most one route runner; senders may still share it

**Dedicated-ingress sessions fail during preflight.** When a factory declares
`CapDedicatedIngressSession`, `bridge.Builder.Plan` counts receivers by session
and route-runner consumers by receiver. It rejects either a second logical
receiver or reuse of the sole receiver by multiple routes before opening stores,
sessions, receivers, senders, or runtime resources. This validation is
capability-based: it does not switch on a transport name, and registering the
same adapter under aliases does not change the session cardinality. Stateful
adapters should also enforce the contract defensively in `NewReceiver` with a concurrency-safe reservation stored
on the `Session`, not on the `Factory`; a factory-local reservation can be
bypassed by aliases or multiple factory values. Do not count `Sender`s in that
reservation.

**Exclusive sessions are single-use.** A transport that declares
`CapExclusiveIdentity` must treat `Start`-after-`Close` as a permanent error:
once `Close` runs, a later `Start` returns `shared.ErrUnavailable` rather than
reconnecting. The runtime depends on this -- when a lease-owning session cannot
renew, it escalates to a terminal state (`ErrSessionUnrecoverable`), releases the
lease so a standby takes over, and lets the orchestrator restart the process with
a fresh session. A transport that silently reconnected a closed exclusive session
would break lease fencing. `CapExclusiveIdentity` is declared by paho MQTT
(always) and amqp091 (latched on first exclusive use); amqp10 does not advertise
the capability but is subject to the same single-use rule when run as an
exclusive session.

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
