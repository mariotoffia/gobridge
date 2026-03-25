# Proposed Cluster And Failover Architecture

This document describes how the proposed GoBridge architecture behaves in clustered deployments.

Related documents:

- [ARCHITECTURE_NEW.md](./ARCHITECTURE_NEW.md)
- [ARCHITECTURE_NEW-TRANSPORTS.md](./ARCHITECTURE_NEW-TRANSPORTS.md)
- [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md)
- [ARCHITECTURE_NEW-EXAMPLES.md](./ARCHITECTURE_NEW-EXAMPLES.md)
- [ARCHITECTURE_NEW-MODULES.md](./ARCHITECTURE_NEW-MODULES.md)
- [ARCHITECTURE_NEW-STORES.md](./ARCHITECTURE_NEW-STORES.md)
- [ARCHITECTURE_RECORDS.md](./ARCHITECTURE_RECORDS.md)

## Purpose

Cluster support must answer two separate questions:

1. How does the bridge fail over with minimal loss?
2. How does the bridge scale out when the source and target have different ownership rules?

The proposal handles this by separating:

- source ownership
- bridge message ownership
- target session ownership

That separation is the key simplification.

## Cluster Primitives

The clustered runtime uses four primitives.

### Lease Store

Used for single-active ownership.

Examples:

- exclusive MQTT `ClientID`
- Kinesis shard worker assignment
- active owner of an outbox partition

### Durable Outbox

Used when the bridge must durably own a message before the target has accepted it.

Examples:

- SQS -> MQTT
- Kinesis -> MQTT
- any route where the source is durable and the target is intermittent

### When Outbox Is Required vs Optional

#### Outbox Required (SharedOutbox mode)

The shared outbox is required when any of the following apply:

- **Decoupled ownership**: Source consumer and target sender may be different bridge instances.
- **Exclusive session handoff**: Target MQTT session uses `SessionMode=Exclusive` with lease-based ownership. A different bridge instance may need to drain the outbox after lease transfer.
- **Fan-out**: `DestinationResolver` returns multiple dispatch plans for one envelope. All plans must be persisted atomically before source ack.
- **Long outages**: Target may be unavailable longer than the source visibility timeout. Without the outbox, the source would redeliver indefinitely or eventually move the message to its own native DLQ.
- **Restart durability**: Messages must survive bridge process restarts after source ack.

#### Outbox Optional (DirectHold mode allowed)

The outbox may be skipped when all of these conditions are met:

- **Single-binding**: `DispatchMode=Single`, no fan-out.
- **Co-located**: Consumer and sender run on the same bridge instance.
- **No lease handoff**: Target session is not exclusive, or is standalone mode.
- **Visibility extension**: Source supports `Delivery.Extend()` (e.g., SQS `ChangeMessageVisibility`).
- **Reliable target**: Target is expected to be consistently available.

In `DirectHold` mode, the source message visibility is extended while processing. If the target rejects or the bridge crashes, the source will redeliver. This trades cross-instance failover capability for lower latency.

See the delivery mode validation rules in [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md#delivery-modes).

### Checkpoint Or Ack Boundary

Every source has a concrete handoff point:

- SQS: delete message
- Azure Service Bus: complete message
- Kinesis: checkpoint shard sequence
- MQTT: acknowledge delivery progression according to QoS/session behavior

The rule is always the same:

- do not cross the source handoff boundary until the next durable owner is known

### Destination Binding Registry

Used when one route may send to many concrete targets.

Examples:

- message A goes to MQTT client `factory-a` topic `devices/1/state`
- message B goes to MQTT client `factory-b` topic `devices/9/state`
- message C goes to Azure Service Bus topic `orders`

## Ownership Model

The runtime tracks three different owners.

### Source Owner

The source owner may receive and progress source state.

Examples:

- one SQS message may be visible to one worker at a time
- one Kinesis shard is owned by one active worker at a time
- one MQTT shared subscription message is assigned by the broker to one active client

### Bridge Owner

The bridge owner is the outbox partition that durably stores the message after source handoff.

This is what allows restart-safe replay.

### Session Owner

The session owner holds the lease for an exclusive target identity.

Examples:

- MQTT `ClientID`
- any stateful target session that must be single-owner

These owners are often different.

That is normal.

## Scaling Patterns

### Pattern 1: Queue Source To Stateless Or Multi-Writer Target

Example:

- SQS -> Azure Service Bus

Behavior:

- many bridge instances consume concurrently from SQS
- any bridge instance may send to Azure Service Bus
- source ack occurs after target acceptance or after outbox persist, depending on policy

This is the easiest case because there is no exclusive target identity.

### Pattern 2: Queue Source To Exclusive MQTT Target

Example:

- SQS -> MQTT where a named `ClientID` must be unique

Behavior:

- many bridge instances may consume from SQS
- all instances may process and classify messages
- messages are persisted to outbox partitions keyed by target session
- only the bridge instance holding the MQTT session lease may drain that outbox partition

This is the critical pattern for clustered scale-out with single-active MQTT publishing.

### Pattern 3: Partitioned Source To Exclusive MQTT Target

Example:

- Kinesis -> MQTT

Behavior:

- shard leases determine which bridge instance reads each shard
- each shard worker persists outbound messages to the outbox
- outbox partitions are keyed by target session identity
- MQTT session leases determine which bridge instance may publish through each exclusive client

This means:

- source scale-out is bounded by shard count
- target scale-out is bounded by number of target sessions
- those two scaling axes are independent

Kinesis is not currently implemented in this repository, but the proposed ownership model fits it naturally.

### Pattern 4: MQTT Shared Subscription To Multi-Writer Target

Example:

- MQTT 5 shared subscription -> SQS or Azure Service Bus

Behavior:

- broker distributes messages across bridge instances
- bridge instances process independently
- target accepts messages durably
- source progression happens after durable target acceptance

No cluster lease is needed for the MQTT subscription itself because the broker already owns work distribution.

## Destination Binding

The proposed architecture needs one explicit abstraction for "send via this target and this client to this address".

Conceptually:

```go
type DestinationBinding struct {
    ID        string
    Transport string
    SessionID string
    SenderID  string
    Address   string
    Options   map[string]any
}

type DispatchPlan struct {
    BindingID string
    Address   string
    Headers   map[string]any
}

type DestinationResolver interface {
    Resolve(ctx context.Context, env *Envelope) ([]DispatchPlan, error)
}
```

Interpretation:

- `BindingID` selects a configured target binding.
- `SessionID` identifies the connection identity when the target is stateful.
- `Address` is the concrete topic, queue, or entity.
- `SenderID` selects the sender implementation when one route may target multiple transports.

This should be a route-level abstraction, not a transport-specific trick hidden inside one sender.

## Why This Matters

Without `DestinationBinding`, the cluster cannot answer:

- which MQTT client lease is required for this message?
- which outbox partition should store the message?
- which topic or target entity should be used on replay?

With `DestinationBinding`, all three questions have a deterministic answer.

## Concrete Example: SQS To The Right MQTT Client And Topic

Assume one SQS queue contains normalized work orders for many factories.

Each factory must publish through its own MQTT client identity:

- factory `A` uses `ClientID=factory-a`
- factory `B` uses `ClientID=factory-b`

Each factory also has a specific topic pattern:

- factory `A`: `factory/a/orders/{device_id}`
- factory `B`: `factory/b/orders/{device_id}`

Conceptual bindings:

```go
bindings := []DestinationBinding{
    {
        ID:        "mqtt-factory-a-orders",
        Transport: "mqtt",
        SessionID: "mqtt-client-factory-a",
        SenderID:  "mqtt-publisher",
        Address:   "factory/a/orders/{device_id}",
        Options: map[string]any{
            "qos": 1,
        },
    },
    {
        ID:        "mqtt-factory-b-orders",
        Transport: "mqtt",
        SessionID: "mqtt-client-factory-b",
        SenderID:  "mqtt-publisher",
        Address:   "factory/b/orders/{device_id}",
        Options: map[string]any{
            "qos": 1,
        },
    },
}
```

Conceptual resolver:

```go
func Resolve(ctx context.Context, env *Envelope) ([]DispatchPlan, error) {
    factory := env.Headers["factory"].(string)
    deviceID := env.Headers["device_id"].(string)

    switch factory {
    case "A":
        return []DispatchPlan{{
            BindingID: "mqtt-factory-a-orders",
            Address:   "factory/a/orders/" + deviceID,
        }}, nil
    case "B":
        return []DispatchPlan{{
            BindingID: "mqtt-factory-b-orders",
            Address:   "factory/b/orders/" + deviceID,
        }}, nil
    default:
        return nil, fmt.Errorf("no route for factory %q", factory)
    }
}
```

Runtime flow:

1. any bridge instance receives the SQS message
2. processors normalize and validate the envelope
3. `DestinationResolver` resolves the concrete binding and topic
4. the bridge persists an outbox record keyed by the chosen `BindingID` and `SessionID`
5. the bridge acks the SQS message
6. the bridge instance that currently holds the lease for that MQTT session drains the outbox partition
7. it publishes to the resolved topic through the correct connected client
8. on broker acceptance, the outbox record is deleted

That is how `SQS -> right connected client -> target & topic` should work.

## Concrete Example: One Source To MQTT And Azure Service Bus

One route may also fan out to different transport families.

Example:

- urgent orders go to MQTT client `factory-a`
- audit copies go to Azure Service Bus topic `orders-audit`

The resolver returns two `DispatchPlan` values:

- one for the MQTT binding
- one for the Azure binding

The runtime persists one outbox record per dispatch plan.

Each record is replayed by the correct sender owner:

- MQTT records by the bridge that owns the MQTT session lease
- Azure records by any bridge instance with sender capacity

Source handoff occurs only after all required outbox records are durable.

### Fan-Out Atomicity

All outbox records for a single fan-out resolution must be persisted atomically. If any dispatch plan fails to persist, none of the records for that envelope must be visible for replay.

Without atomicity, a partial persist followed by a crash creates orphan records: the source will redeliver, creating duplicate outbox entries for the dispatch plans that succeeded on the first attempt.

Rules:

- `OutboxStore.Persist` must accept a slice of `OutboxRecord` and persist them as a single atomic operation.
- For DynamoDB, use `TransactWriteItems` for multi-record fan-out persists. DynamoDB transactions support up to 100 items, which is the practical ceiling on fan-out cardinality per envelope.
- If the store does not support atomic multi-item writes, the runtime must either retry the entire batch or use a two-phase approach: write a fan-out intent record first, then expand into individual dispatch records, and only ack the source after the intent is durable.
- The startup validator should reject routes where `DispatchMode=FanOut` and the configured `OutboxStore` does not support atomic batch writes with at least the maximum expected fan-out cardinality.

## Outbox Replay Ordering

Outbox replay within a partition follows insertion order (FIFO by `created_at`). This preserves ordering semantics for transports that guarantee ordering.

If the outbox store cannot guarantee FIFO within a partition, this is a known limitation. Routes that require strict ordering should use transports with native ordering guarantees (SQS FIFO with message group IDs, Azure Service Bus sessions).

Message ordering is not guaranteed during failover. When a lease transfers, messages processed by the crashed instance may be replayed from the outbox interleaved with new messages processed by other instances. For order-sensitive routes, consumers should implement sequence-number-based reordering.

## DispatchPlan Header Merge Semantics

When a `DestinationResolver` returns `DispatchPlan` values with `Headers`, those headers are merged into a copy of the `Envelope.Headers` for that specific dispatch. The merge follows these rules:

- `DispatchPlan.Headers` values take precedence on key collision.
- The original `Envelope.Headers` remain unmodified (the merge operates on a copy).
- Standard headers with the `x-bridge.` prefix set by the runtime (correlation ID, route ID, source ID) cannot be overridden by `DispatchPlan.Headers`.
- Transport adapters receive the merged header map for each dispatch.

## Cross-Instance Backpressure

When ingress and egress are decoupled across bridge instances, the outbox can grow without limit if the target is down. The architecture must prevent unbounded outbox growth.

Rules:

- Each route defines a `MaxOutboxDepth` in its route policy.
- The outbox store exposes a `Depth(ctx, partitionKey)` method or equivalent metric.
- When outbox depth exceeds the configured threshold, the runtime applies source-side backpressure: reduce SQS polling rate, block `Delivery` ack, or use MQTT `Receive Maximum` reduction.
- Outbox depth per partition is exposed as a metric for monitoring and alerting.

For cross-instance coordination, outbox depth should be readable from the shared store. Ingress instances can read the depth for their target partition and self-throttle when it exceeds a configurable percentage of `MaxOutboxDepth`.

## Circuit Breakers For Stores

`OutboxStore` and `LeaseStore` operations should be wrapped with circuit breakers. If the backing store becomes intermittently available:

- Open circuit on the outbox store should cause the route to enter a "paused" state where source intake is blocked but existing outbox entries are not lost.
- Open circuit on the lease store should trigger step-down behavior: the bridge stops claiming new outbox entries but completes in-flight ones.
- Circuit breaker state transitions should be logged as structured events and exposed as metrics.

## Health Aggregation

Route-level health is the composite of:

- source health (receiver connectivity)
- session health (MQTT connection state)
- outbox store health (write/read availability)
- lease health (renewal success rate)

This composite health feeds into:

- readiness probes (route is ready to process messages)
- lease release decisions (unhealthy session may trigger voluntary lease release)
- operational dashboards and alerting

## Outbox Partitioning

For clustered correctness, the outbox cannot be only process-local when ingress and egress ownership are decoupled.

The durable key should include at least:

- route ID
- source message ID or derived bridge message ID
- binding ID
- session ID when relevant

Recommended partition strategy:

- partition by `session ID` for exclusive MQTT targets
- partition by `binding ID` for stateless targets

That makes lease transfer and replay deterministic.

## Failure Scenarios

### Failure 1: Consumer Dies Before Outbox Persist

Result:

- source still owns the message
- another bridge instance receives it again
- no message is lost

### Failure 2: Consumer Dies After Outbox Persist But Before Source Ack

Result:

- source may redeliver
- outbox already contains the message
- `OutboxStore.Persist` rejects the duplicate via conditional write on `EnvelopeID + BindingID`
- if the conditional write fails with a duplicate key error, the runtime treats the persist as successful (the message is already in the outbox) and proceeds to ack the source

This is handled by the idempotency contract defined in [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md).

### Failure 3: Consumer Dies After Source Ack But Before Target Send

Result:

- outbox owns the message
- another bridge instance replays it from the outbox
- no message is lost if the outbox is durable and shared correctly

### Failure 4: MQTT Publisher Dies While Holding An Exclusive Session Lease

Result:

- lease expires or is transferred
- another bridge instance acquires the lease
- it reconnects with the same `ClientID`
- it reconciles session state
- it resumes draining that session's outbox partition

## Scalability Limits

The design scales, but the limit comes from the transport.

- SQS scale-out is driven by consumer concurrency and queue throughput.
- Kinesis scale-out is driven by shard count.
- MQTT shared subscription scale-out is driven by broker support and broker limits.
- Exclusive MQTT publishing scale-out is driven by number of distinct session identities, not by number of bridge instances alone.

Adding more bridges does not increase throughput for one exclusive MQTT `ClientID`.

It does increase:

- failover capacity
- source-side parallelism
- total throughput across many target sessions

## Delivery Guarantees

The proposed clustered runtime aims for:

- at-least-once delivery across failover boundaries
- deterministic ownership transfer
- minimal loss when leases move or processes crash

It does not claim:

- exactly-once end-to-end delivery across heterogeneous transports
- lossless delivery without durable source or durable outbox support

## Lease Fencing And Split-Brain Prevention

The lease system must prevent a stale owner from continuing to operate after ownership has transferred. Without fencing, a GC pause or network partition can cause two bridge instances to believe they hold the same lease simultaneously.

### Fencing Token Protocol

Every `Lease.Acquire` and `Lease.Renew` returns a `LeaseToken` with a monotonically increasing `Version`. This version acts as a fencing token throughout the outbox drain pipeline.

Rules:

- `OutboxStore.Claim` must accept the caller's fencing token and store it on the claimed entry.
- `OutboxStore.Complete` must validate that the caller's fencing token matches the claim token.
- A stale fencing token must be rejected atomically by the store.
- The session owner must verify its fencing token against the `LeaseStore` before each drain batch.
- On any fencing token mismatch, the bridge must immediately stop sending, disconnect the MQTT session, and re-enter lease acquisition.

### Lease Renewal Failure Semantics

If lease renewal fails, the bridge should follow a three-phase policy:

1. **Retry**: Retry renewal with backoff, up to 3 attempts within the lease TTL.
2. **Step-down**: If all retries fail, stop claiming new outbox entries but continue completing in-flight entries.
3. **Release**: After a grace period, release the lease and disconnect the session.

The lease TTL should be at least 3x the renewal interval to allow time for transient failures.

## Recommended Defaults

- use a durable shared outbox for any route that bridges from durable ingress to intermittent or exclusive egress
- use lease-based session ownership for exclusive MQTT clients
- use shared subscriptions for MQTT ingress only when the broker supports them
- align MQTT 5 `Receive Maximum` with route in-flight limits
- require idempotency keys for all cluster-safe reliable routes
