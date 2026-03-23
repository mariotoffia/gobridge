# Proposed Transport Architecture

This document defines how transports should fit into the proposed GoBridge architecture.

Related documents:

- [ARCHITECTURE_NEW.md](./ARCHITECTURE_NEW.md)
- [ARCHITECTURE_NEW-CLUSTERING.md](./ARCHITECTURE_NEW-CLUSTERING.md)
- [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md)
- [ARCHITECTURE_NEW-EXAMPLES.md](./ARCHITECTURE_NEW-EXAMPLES.md)
- [ARCHITECTURE_NEW-MODULES.md](./ARCHITECTURE_NEW-MODULES.md)
- [ARCHITECTURE_NEW-STORES.md](./ARCHITECTURE_NEW-STORES.md)
- [ARCHITECTURE_RECORDS.md](./ARCHITECTURE_RECORDS.md)

## Transport Roles

Each transport implementation should expose only the roles it actually needs:

- `Receiver`
- `Sender`
- `Session` when transport state spans reconnects

Examples:

- SQS receive-only route: `Receiver`
- SNS or SQS send-only route: `Sender`
- MQTT subscribe/publish route: `Session` + `Receiver` + `Sender`
- Azure Service Bus receive/send route: `Receiver` + `Sender`, optionally with transport-native sessions for ordering

## Minimal Transport Contract

Conceptually:

```go
type SessionFactory interface {
    NewSession(ctx context.Context, spec SessionSpec) (types.Session, error)
}

type ReceiverFactory interface {
    NewReceiver(ctx context.Context, spec ReceiverSpec, session types.Session) (types.Receiver, error)
}

type SenderFactory interface {
    NewSender(ctx context.Context, spec SenderSpec, session types.Session) (types.Sender, error)
}
```

Rules:

- `session` may be `nil` for stateless transports.
- receiver and sender specs must not duplicate session settings.
- transport packages should normalize transport-specific metadata into `Envelope.Headers`.

## Recommended Capability Model

The current capability model is too granular for the core runtime.

The proposed capability set should answer only routing-relevant questions:

| Capability | Meaning |
|------------|---------|
| `StatefulSession` | Transport maintains remote session state across reconnects |
| `SourceRedelivery` | Source can retry or make message visible again |
| `VisibilityExtension` | Source can extend processing window |
| `DelayedSend` | Sender can schedule delivery for the future |
| `SharedConsumer` | Transport can safely distribute one logical subscription across clients |
| `ExclusiveIdentity` | Transport identity must be single-owner |

Everything else should remain transport detail.

## MQTT Design

MQTT is the transport that most strongly shapes the redesign because its state lives at the session layer.

### MQTT 3.1.1 And MQTT 5

| Feature | MQTT 3.1.1 | MQTT 5 |
|--------|-------------|--------|
| Persistent session | Yes, via `CleanSession=0` | Yes, via `Clean Start` + `Session Expiry Interval` |
| Message expiry | No | Yes |
| Shared subscriptions | Not in standard spec | Yes |
| No Local | No | Yes |
| Retain handling controls | No | Yes |
| Receive maximum | No | Yes |
| Explicit disconnect reason | Limited | Yes |
| Server redirection | No standard mechanism | Yes |

Design rule:

- model MQTT 5 as the primary target
- degrade gracefully to MQTT 3.1.1 where features are unavailable

### MQTT Session

An MQTT session owns:

- broker URLs
- transport credentials
- `ClientID`
- keep alive
- clean start / clean session behavior
- session expiry
- remote subscriptions
- in-flight QoS 1 and 2 protocol state

That means:

- `ClientID` is never endpoint-local
- subscriptions are never independent from the session
- reconnect must always run through session reconciliation

### MQTT Session Modes

#### Ephemeral Session

Use for telemetry and simple live-only routing.

- MQTT 3.1.1: `CleanSession=1`
- MQTT 5: `Clean Start=1`, `Session Expiry Interval=0`

Effects:

- bridge must resubscribe after every reconnect
- no remote buffering while disconnected
- failover starts from empty session state

#### Persistent Session

Use for durable subscription continuity.

- MQTT 3.1.1: `CleanSession=0`
- MQTT 5: `Clean Start=0` or first connect with `Clean Start=1`, then non-zero session expiry

Effects:

- broker retains subscription state
- broker may retain queued QoS 1 and 2 messages while the client is offline
- failover should reconnect with the same `ClientID`

#### Exclusive Session

Use when only one bridge instance may connect with the named `ClientID`.

Effects:

- requires lease ownership in cluster mode
- new owner reconnects with the same `ClientID`
- broker disconnects the old owner if still connected

This is the correct answer to "how do we route to topics when only one client at a time may connect?"

The bridge should not attempt active/active publishing with the same exclusive identity.

### MQTT Subscription Types

#### Non-shared Subscription

Every subscribed session receives its own copy.

Use when:

- each bridge needs a full copy
- a route is single-active

#### Shared Subscription

MQTT 5 shared subscriptions distribute matching publications across subscribing sessions.

Use when:

- multiple bridge instances may process the same logical subscription
- horizontal scale-out is desired

Design rule:

- if MQTT shared subscriptions are supported, let the broker perform distribution
- if they are not supported, use single-active lease ownership instead

The cluster should coordinate membership, not emulate broker-side load balancing.

### MQTT Subscription Options

MQTT 5 subscription options should be represented in the receiver spec:

- maximum QoS
- no local
- retain as published
- retain handling
- subscription identifier when supported

These are part of desired session state and belong in reconciliation.

### MQTT Publishing

Publishing rules:

- QoS 0 is best-effort only
- QoS 1 means broker accepted the publish when `PUBACK` is received
- QoS 2 means broker accepted the publish when `PUBCOMP` is received

Implications for bridging:

- do not ack an upstream durable source merely because a QoS 0 write was attempted
- use QoS 1 or 2 for reliable bridge egress
- if application restart durability matters, persist to outbox before acking the source

### MQTT Message Expiry

MQTT 5 supports broker-side message expiry.

Design rule:

- bridge expiry is `Envelope.ExpiresAt`
- MQTT `Message Expiry Interval` is derived from remaining lifetime
- never write the original requested TTL after time has already elapsed

MQTT 3.1.1 has no standard message expiry, so the bridge must enforce expiry locally.

### MQTT Receive Maximum And Backpressure

MQTT 5 `Receive Maximum` is the protocol-level equivalent of in-flight limit for QoS 1 and 2 messages.

Design rule:

- bridge backpressure and MQTT receive maximum should be aligned
- reliable modes must not drop locally because an internal channel is full
- local channel overflow is a correctness bug for QoS 1 and 2 bridging

### MQTT Reconnect And Reconciliation

On reconnect, the session should:

1. inspect broker response and session present state
2. determine whether prior session state still exists
3. reconcile desired subscriptions
4. resume outbox replay

That is simpler and safer than having each source object subscribe independently.

### MQTT Failover Patterns

#### Pattern 1: Single-Active Named Client

Use when:

- broker policy requires a single named client
- upstream system identifies the bridge by `ClientID`

Requirements:

- cluster lease
- persistent session when continuity matters
- reconnect with same `ClientID`

#### Pattern 2: Shared Consumer Group

Use when:

- broker supports shared subscriptions
- work can be distributed across multiple bridge instances

Requirements:

- one shared subscription name per logical consumer group
- no local lease needed for that subscription

#### Pattern 3: Broker Failover

Use when:

- multiple broker URLs are configured
- broker returns server redirection

Requirements:

- broker URL list belongs to the session spec
- reconnect logic retries alternate URLs
- redirection information may update broker choice, but not route ownership

### MQTT 3.1.1 Degradation Matrix

When connecting to an MQTT 3.1.1 broker, the following features are unavailable and the bridge degrades as follows:

| Feature | MQTT 5 Behavior | MQTT 3.1.1 Fallback | Resilience Impact |
|---------|-----------------|---------------------|-------------------|
| Receive Maximum | Aligned with route in-flight limits | Not available; bridge must enforce backpressure locally via channel limits | Increased risk of local buffer overflow; reliable routes must block |
| Message Expiry | Derived from `Envelope.ExpiresAt` and set on publish | Not available; bridge enforces expiry locally before send | Broker may retain expired messages in persistent sessions |
| Shared Subscriptions | Broker distributes across consumers | Not standard; fall back to single-active lease ownership | Reduced horizontal scale-out; requires lease infrastructure |
| Session Expiry Interval | Configurable expiry after disconnect | Session lifetime is broker-controlled | Less predictable session cleanup |
| No Local | Prevents receiving own publishes | Not available; bridge must filter locally if needed | Possible message loops if not handled |
| Disconnect Reason | Explicit reason code | Not available; bridge logs disconnect without cause | Harder failure diagnosis |

The bridge must emit a startup warning when connecting to an MQTT 3.1.1 broker listing features that cannot be supported.

## AWS SQS And SNS

### SQS Receiver

SQS is a natural `Receiver`:

- `Ack()` deletes message
- `Retry()` makes message visible again, optionally with delay via policy or re-enqueue
- `Extend()` renews visibility timeout

It is queue-oriented, so it normally does not require a session abstraction.

### SQS Sender

SQS is a natural `Sender`:

- accepts messages durably
- supports delayed send
- FIFO queues add ordering and deduplication constraints

Recommended mapping:

- `Envelope.Subject` maps to an attribute or logical route field
- `Envelope.Headers` maps to message attributes
- ordering key and deduplication key should be explicit headers

### SNS

SNS is not a durable bridge ingress by itself when it pushes directly to a bridge process.

Recommended rule:

- if reliable ingress is required, terminate SNS into SQS first
- bridge from SQS, not from direct HTTP push

That gives clear ownership and replay semantics.

## Azure Service Bus

Azure Service Bus maps naturally to receiver and sender roles.

### Receiver

- `Ack()` completes the message
- `Retry()` abandons or dead-letters depending on policy
- `Extend()` renews lock

### Sender

- durable accept boundary is sender acceptance
- delayed delivery maps to scheduled enqueue

### Service Bus Sessions

Service Bus sessions are ordering and affinity constructs, not the same concept as an MQTT connection session.

Design rule:

- treat Service Bus `SessionID` as message or receiver affinity metadata
- do not force Service Bus sessions into the same semantics as MQTT session ownership

## Implementing A New Transport

### Step 1: Decide Whether The Transport Is Stateful

Questions:

- does the transport maintain remote state across reconnects?
- does a network identity own subscriptions or pending messages?
- can multiple endpoints safely share one remote session?

If yes, implement a `Session`.

### Step 2: Normalize Into Envelope

Transport adapters must populate:

- `ID`
- `Subject`
- `Payload`
- `Headers`
- `CreatedAt`
- `ExpiresAt` when available

### Step 3: Implement Delivery Semantics

The transport receiver must define:

- ack behavior
- retry behavior
- extend behavior

These are source semantics, not middleware behavior.

If the transport does not support a `Delivery` operation (e.g., MQTT cannot extend a processing deadline), the implementation should return `ErrNotSupported`. The runtime checks for this error and falls back to alternative behavior (e.g., relying on backpressure instead of visibility extension).

### Step 4: Keep Endpoint Specs Small

Receiver and sender specs should contain only endpoint-local settings.

Do not put:

- broker URLs
- transport credentials
- session identity

on every endpoint if a session already owns them.

## Optional BatchSender

Transports that support batch operations (SQS, Azure Service Bus) should implement the optional `BatchSender` interface in addition to `Sender`:

```go
type BatchSender interface {
    Sender
    SendBatch(ctx context.Context, envs []*Envelope) (int, error)
}
```

The runtime should check for `BatchSender` at wiring time and use batch sends for outbox drain when available.

## Session State Notifications

The `Session` interface includes an `Events()` channel that the route runner uses to receive state change notifications:

```go
type SessionEventType int

const (
    SessionConnected    SessionEventType = iota
    SessionDisconnected
    SessionReconnecting
    SessionError
)
```

This replaces polling `Health()` and enables prompt reaction to disconnects, reconnects, and errors.

## Anti-Patterns

- Per-source subscribe and unsubscribe calls on a shared MQTT connection.
- Multiple endpoint objects each carrying their own `ClientID`.
- Local message drops in reliable modes.
- Duplicating transport retry logic in middleware.
- Treating failover as "move source objects" instead of "transfer session ownership".
