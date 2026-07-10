# Scenario 5: Durable Delivery with SharedOutbox

Guarantee zero message loss across bridge crashes by decoupling ingress from egress with a persistent outbox.

## Use Case

IoT sensor data arrives on MQTT and must reach an SQS queue for downstream processing. If the bridge crashes between receiving an MQTT message and sending it to SQS, the message must not be lost. The default `direct_hold` mode holds the source delivery open during send, but if the process dies mid-flight, the message is gone -- the MQTT broker already delivered it, and the SQS send never completed.

The `shared_outbox` delivery mode solves this by persisting messages to a durable store before acknowledging the source. A background drainer then reads from the outbox and delivers to the target. If the bridge crashes, the drainer picks up persisted records on restart.

## Architecture

```mermaid
flowchart LR
    subgraph MQTT Broker
        T["sensors/#"]
    end

    subgraph GoBridge
        R[Receiver\nmqtt-in]
        Route[Route\nsensor-ingest\ndelivery: shared_outbox]
        OB[(Outbox Store)]
        DR[Outbox Drainer]
        S[Sender\nsqs-out]
    end

    subgraph AWS
        Q["SQS Queue\nsensor-events"]
    end

    T -->|subscribe| R
    R -->|1. receive| Route
    Route -->|2. persist| OB
    OB -->|3. ACK source| R
    DR -->|4. claim records| OB
    DR -->|5. send| S
    S -->|6. SendMessage| Q
    DR -->|7. complete| OB

    style Route fill:#f96,stroke:#333
    style OB fill:#fcf,stroke:#333
    style DR fill:#cff,stroke:#333
```

## Direct Hold vs Shared Outbox

The two delivery modes represent different trade-offs.

```mermaid
flowchart TD
    subgraph "direct_hold"
        DH1[Receive message] --> DH2[Send to target]
        DH2 --> DH3{Success?}
        DH3 -->|Yes| DH4[ACK source]
        DH3 -->|No| DH5[Retry or\nreject source]
        DH6["Crash window"] -.->|Process dies here\nMessage lost| DH2
    end

    subgraph "shared_outbox"
        SO1[Receive message] --> SO2[Persist to outbox]
        SO2 --> SO3[ACK source]
        SO3 --> SO4[Drainer claims record]
        SO4 --> SO5[Send to target]
        SO5 --> SO6[Complete record]
        SO7["Crash window"] -.->|Process dies here\nOutbox survives| SO4
    end

    style DH6 fill:#fcc,stroke:#c33
    style SO7 fill:#cfc,stroke:#3c3
```

With `direct_hold`, the source delivery stays open while the target send runs. Fast and simple, but a crash between receive and ACK means the message is in limbo. MQTT QoS 1 will redeliver, but there is a window where the broker may consider the message delivered if the TCP connection was already clean.

With `shared_outbox`, the message is persisted before the source is acknowledged. The outbox survives crashes. After restart, the drainer finds pending records and delivers them.

## Configuration

```yaml
bridge:
  id: durable-sensor-ingest

sessions:
  - id: mqtt-conn
    transport: mqtt
    options:
      session:
        broker_url: tcp://mqtt.example.com:1883
        client_id: durable-ingest-01
        keep_alive: 30

stores:
  outbox:
    type: sqlite
    options:
      path: /var/lib/gobridge/outbox.db
  lease:
    type: memory # single-instance example; use dynamodb for multi-instance (see stores.lease)
    options:
      # The in-memory lease keeps ownership per-process and cannot coordinate
      # across replicas, so single-replica operation must be acknowledged
      # explicitly before it will build. Use dynamodb for multi-instance.
      acknowledge_single_replica: true

receivers:
  - id: mqtt-in
    session_id: mqtt-conn
    topics:
      - topic: "sensors/#"
        qos: 1

senders:
  - id: sqs-out
    transport: sqs
    options:
      queue_url: https://sqs.us-west-1.amazonaws.com/123456789/sensor-events
      region: us-west-1
      batch_size: 10

bindings:
  - id: to-sqs
    sender_id: sqs-out
    address: sensor-events

routes:
  - id: sensor-ingest
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    dispatch_mode: single
    bindings: [to-sqs]
    policy:
      ack_after: outbox_persist
      max_in_flight: 100
      max_outbox_depth: 10000
      max_replay_attempts: 5
      on_expired: dlq
      on_permanent_failure: dlq
    session:
      session_id: mqtt-conn
      sender_id: sqs-out
      drain_interval: 1s
      drain_batch_size: 50
```

> **Note on the SQS binding `address`.** The SQS sender is pinned to one queue via its `queue_url` or `queue_name`. The binding `address` may be the bare queue name (as here, `sensor-events`) or the full queue URL -- either form is matched to that bound queue rather than routing per message.

## Config Walkthrough

### `delivery_mode: shared_outbox`

Switches the route from synchronous hold to asynchronous outbox-based delivery. The route pipeline persists the envelope into the outbox store instead of sending directly to the target. A background drainer process handles actual delivery.

Config validation (`ValidateBlueprintGraph`, run on every load) enforces two rules for a `shared_outbox` route:

- `stores.outbox` must be configured.
- If the route binds to an **exclusive** session, `stores.lease` must be configured -- exclusive drain needs a lease to fence single ownership.

Outbox delivery is at-least-once, so design the downstream for deduplication: give each envelope a stable `Envelope.ID` (from the source) or stamp one with an idempotency-key processor. This is a design expectation, not a load-time check.

### `ack_after` -- When to Acknowledge the Source

For a `shared_outbox` route the acknowledgement boundary is fixed: the source is
ACKed once the outbox write succeeds. The durable outbox record IS the guarantee,
so there is nothing stronger to wait for.

| Value | Behavior | Notes |
|-------|----------|-------|
| `outbox_persist` | ACK the source as soon as the outbox write succeeds | The default, and the only accepted value, for `shared_outbox`. Fast ACK; the message survives a crash once it is in the outbox. |
| `target_accept` | ACK only after the target sender confirms delivery | **Rejected on a `shared_outbox` route** -- the runtime fails validation at startup (`runtime/validator.go:278-286`). It is the `direct_hold` default, where there is no outbox to persist to. |

With `outbox_persist`, the MQTT PUBACK is sent the moment the outbox store confirms
the write. The message is durable in the outbox but has not yet reached SQS. This
keeps ingress latency low and is the boundary `shared_outbox` is built around.

Setting `ack_after: target_accept` on a `shared_outbox` route is a startup error,
not a stronger guarantee -- the drainer sends to the target asynchronously, so the
source ACK can never be deferred to the target. If you need the source held open
until the target accepts, use `delivery_mode: direct_hold` instead; that trades the
outbox's crash durability for end-to-end confirmation before ACK. Omitting
`ack_after` on a `shared_outbox` route is the safe choice: it defaults to
`outbox_persist`.

### `stores.outbox`

The outbox store must be configured when using `shared_outbox` delivery mode.

```yaml
stores:
  outbox:
    type: sqlite
    options:
      path: /var/lib/gobridge/outbox.db
```

The `type` field selects the store backend:

| Type | Durability | Use Case |
|------|-----------|----------|
| `memory` | Process lifetime only | Development, testing, low-risk workloads |
| `sqlite` | Disk-persistent | Single-instance production without external DB |
| `dynamodb` | Cloud-durable | Multi-instance production, high availability |

For true crash survival, use `sqlite` or `dynamodb` -- this scenario's examples default to `sqlite` for exactly that reason. The `memory` store is development-only: it loses all pending records on process restart, which would violate the zero-loss guarantee this scenario promises.

### `stores.lease`

The lease store coordinates outbox ownership in multi-instance deployments. The drainer acquires a lease before claiming outbox records, preventing duplicate delivery when multiple bridge instances share the same outbox store.

For single-instance deployments, `memory` is sufficient. For multi-instance, use `dynamodb` to ensure only one instance drains at a time.

### `max_outbox_depth`

```yaml
policy:
  max_outbox_depth: 10000
```

Limits the number of pending records in the outbox. When the depth reaches this limit, the route applies backpressure -- new messages from the receiver are blocked until the drainer reduces the queue. The default is 10,000 records.

This protects against unbounded memory growth when the target is down. The receiver pauses accepting deliveries, which in turn applies backpressure to the MQTT broker via QoS flow control.

### `max_replay_attempts`

```yaml
policy:
  max_replay_attempts: 5
```

The minimum number of times the drainer must claim an outbox record before it becomes eligible for poison after a **transient** send failure. Reaching this count is not sufficient to poison a record: the drainer routes it to the DLQ only once all three conditions hold -- the replay count has passed `max_replay_attempts`, the record has a recorded first delivery attempt, and the wall-clock since that first attempt has reached `replay_budget` (default 15m; see below). `max_replay_attempts` is the minimum-attempts floor; `replay_budget` is the wall-clock that bounds total delivery time. Together they stop a transient egress outage -- a broker restart, node replacement, or deploy rollover -- from poisoning otherwise-healthy messages before the budget runs out. A record persisted before the first-attempt schema (no recorded first attempt) falls back to the older age gate measured from `CreatedAt`, so an upgrade never poisons a backlog. A **permanent** (non-transient) send error skips the budget and is DLQ'd on the spot. The drainer is the only component that poisons an outbox record. The default is 5 attempts.

### `replay_budget`

```yaml
policy:
  replay_budget: 15m
```

The wall-clock budget, measured from a record's first delivery attempt, that bounds how long the drainer keeps retrying a **transient** failure before it poisons the record to the DLQ. It sizes for the outages a healthy target recovers from -- a broker restart, node replacement, or deploy rollover -- so those never DLQ a good message before it drains. `max_replay_attempts` sets the attempt floor; this sets the time. Legacy records with no recorded first attempt fall back to the older `CreatedAt` age gate. The default is 15m.

### Outbox Drainer

The drainer is a background goroutine that runs the following loop:

1. **Claim** -- Query the outbox store for pending records, marking them as claimed by this instance
2. **Send** -- Deliver each claimed record to the target sender
3. **Complete** -- Mark successfully sent records as completed
4. **Retry** -- Failed records are released back to pending with an incremented replay count
5. **Wait** -- Sleep for `drain_interval` before the next cycle

#### One drainer per session partition -- align policy or split sessions

A session partition has **exactly one** drainer: the first `shared_outbox` route that references a given session builds it, and every other route that drains the same session shares it. That single drainer applies **one** set of drain-relevant policy to every record in the partition, regardless of which route persisted the record. The drain-relevant policy is `send_timeout`, `max_replay_attempts`, `replay_budget`, `on_expired`, and `on_permanent_failure`.

Because a record is source-ACKed the moment it is persisted, a record persisted by one route may later be drained -- and terminally settled -- under another route's policy. If those policies diverged, that record would be settled under the wrong terminal behavior: for example, a record persisted by an `on_permanent_failure: dlq` route but drained under an `on_permanent_failure: drop` route would be dropped with **no DLQ evidence** after the source was already acknowledged -- silent message loss.

To close that hazard, the runtime **fails closed at validation**: if two or more `shared_outbox` routes drain the same session partition with divergent drain-relevant policy, `ValidateRoutes`/`Start` reject the configuration and name both routes and the shared session. Give the routes their own sessions, or align their drain-relevant policy. Routes that differ only on ingress-side fields (for example `max_in_flight`) may still share a session -- only the drain-relevant policy must match.

### `drain_interval` and `drain_strategy`

The `session.drain_interval` field sets a fixed polling interval:

```yaml
session:
  drain_interval: 1s
```

For more sophisticated polling, use `drain_strategy`:

```yaml
session:
  drain_strategy:
    type: adaptive_backoff
    min_interval: 100ms
    max_interval: 30s
    multiplier: 2.0
```

The `adaptive_backoff` strategy reduces polling frequency when the outbox is empty (saving resources) and increases it immediately when records are found (reducing latency). When `drain_strategy` is set, it takes precedence over `drain_interval`.

| Strategy | Behavior |
|----------|----------|
| `fixed_poll` | Always waits the configured interval between drain cycles |
| `adaptive_backoff` | Starts at `min_interval`, backs off by `multiplier` when empty, resets to `min_interval` when records are found |

## Crash Recovery

The following sequence shows what happens when the bridge crashes and restarts.

```mermaid
sequenceDiagram
    participant MQTT as MQTT Broker
    participant Bridge as GoBridge
    participant Outbox as Outbox Store
    participant SQS as SQS Queue

    Note over Bridge: Normal operation
    MQTT->>Bridge: Deliver message A
    Bridge->>Outbox: Persist record A
    Outbox-->>Bridge: OK
    Bridge->>MQTT: PUBACK (message A)

    MQTT->>Bridge: Deliver message B
    Bridge->>Outbox: Persist record B
    Outbox-->>Bridge: OK
    Bridge->>MQTT: PUBACK (message B)

    Note over Bridge: Drainer claims records
    Bridge->>Outbox: Claim(limit=50)
    Outbox-->>Bridge: Records [A, B]
    Bridge->>SQS: Send A
    SQS-->>Bridge: OK
    Bridge->>Outbox: Complete(A)

    Note over Bridge,SQS: CRASH before sending B

    Note over Bridge: Bridge restarts
    Bridge->>Outbox: Claim(limit=50)
    Outbox-->>Bridge: Records [B] (still pending)
    Bridge->>SQS: Send B
    SQS-->>Bridge: OK
    Bridge->>Outbox: Complete(B)
    Note over SQS: Both A and B delivered
```

Record A was completed before the crash, so it is not re-sent. Record B was claimed but never completed -- the outbox store releases it back to pending status (the claim expires), and the new drainer instance picks it up.

### Transient Failures vs. Crash Recovery

Two distinct paths return a claimed record to `pending`:

- **Transient send failure, owner still alive** (for example, the target broker briefly disconnects). The drainer returns the record to `pending` immediately, so the same owner re-claims and retries it on the very next drain -- no fencing-version bump and no wall-clock wait. This live-owner release is available on the `memory`, `sqlite`, and `dynamodb` stores. Each retry increments the replay count, so a persistently failing record still reaches the DLQ once it exceeds `max_replay_attempts`.
- **Crash recovery, owner gone.** A dead instance cannot release anything. `dynamodb` additionally reclaims a record whose claim has gone stale past a wall-clock threshold, allowing another instance to take over. The `memory` and `sqlite` stores have no wall-clock reclaim; a restarted single instance recovers its own records when it re-acquires the lease at a higher fencing version.

## Key Decisions: When to Use Each Mode

| Criterion | `direct_hold` | `shared_outbox` |
|-----------|--------------|-----------------|
| Simplicity | Simple, no stores needed | Requires outbox + lease stores |
| Latency | Low (synchronous send) | Higher (persist + drain cycle) |
| Crash safety | Message may be lost on crash | Message survives in outbox |
| Throughput | Bounded by target latency | Ingress decoupled from egress |
| Multi-instance | Works independently | Requires lease coordination |
| Resource usage | Minimal | Outbox storage + drainer goroutine |

**Use `direct_hold` when:**
- Messages are non-critical or the source has its own redelivery (e.g., SQS visibility timeout)
- Simplicity is preferred over durability
- Latency is the primary concern

**Use `shared_outbox` when:**
- Zero message loss is a hard requirement
- The target may be temporarily unavailable (outbox buffers during outages)
- You need to decouple ingress throughput from egress latency
- Running multiple bridge instances that must coordinate delivery

## Variations

### SQLite Outbox for Disk Persistence

Replace the in-memory outbox with SQLite for single-instance crash survival:

```yaml
stores:
  outbox:
    type: sqlite
    options:
      path: /var/lib/gobridge/outbox.db
  lease:
    type: memory
    options:
      acknowledge_single_replica: true
```

On crash and restart, the drainer finds all pending records and resumes
delivery. WAL journalling is always enabled. The store keeps a single writer
connection with a `busy_timeout`, so concurrent in-process writers serialise
safely rather than failing with `SQLITE_BUSY`.

**Recovering in-flight (claimed-but-not-completed) records.** A record that was
claimed by a drainer that then crashed before completing or releasing it is
recovered two ways: a higher lease fencing version reclaims it immediately, and
— as a same-owner fallback — the `stale_claim_duration` window (auto-derived
from `step_down_grace`, or set explicitly) lets it be re-claimed once the claim
goes stale. The native SQLite outbox now honours `stale_claim_duration` for this
fallback, matching the DynamoDB backend.

> **Note on `lease: memory`.** The in-memory lease store resets its fencing
> version on restart, so it cannot guarantee version continuity across a crash.
> For a single instance whose lease returns to the same version this is safe and
> the stale-claim fallback above still recovers stranded work; for
> multi-instance or strict crash-recovery guarantees use a durable lease store
> (DynamoDB). `memory` lease and outbox remain development/test grade — see the
> store readiness notes in [config-stores](../config-stores.md).

### DynamoDB Outbox for Multi-Instance

For multi-instance production deployments, both stores must use DynamoDB so that lease coordination and outbox access are shared across instances:

```yaml
stores:
  outbox:
    type: dynamodb
    options:
      table_name: gobridge-outbox
      region: us-west-1
  lease:
    type: dynamodb
    options:
      table_name: gobridge-leases
      region: us-west-1
```

**Standby readiness.** Only the lease holder drains; standby instances hold their drainers idle until they win the lease. The active drainer also gates each cycle on egress-transport readiness -- when the target session is disconnected, it skips the drain instead of running failing Claim+Send cycles. This keeps a broker outage from silently burning the replay budget and poisoning healthy records while the target is simply unreachable.

### Adaptive Backoff Drain Strategy

Reduce DynamoDB read costs during idle periods. When messages flow, the drainer polls every 100ms. When the outbox is empty, it backs off (200ms, 400ms, 800ms, ... up to 30s) and resets to 100ms when records reappear:

```yaml
session:
  drain_batch_size: 100
  drain_strategy:
    type: adaptive_backoff
    min_interval: 100ms
    max_interval: 30s
    multiplier: 2.0
```

### DLQ for Failed Deliveries

A record reaches the DLQ store on a permanent send error (immediately) or on replay-count exhaustion once it has also spent its `replay_budget` -- the wall-clock from the first delivery attempt, default 15m (legacy records without a recorded first attempt fall back to the `CreatedAt` age gate). Operators can inspect and replay entries via the HTTP admin API:

```yaml
stores:
  outbox:
    type: sqlite
    options:
      path: /var/lib/gobridge/outbox.db
  dlq:
    type: sqlite
    options:
      path: /var/lib/gobridge/dlq.db

routes:
  - id: sensor-ingest
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    bindings: [to-sqs]
    policy:
      max_replay_attempts: 3
      on_permanent_failure: dlq
      on_expired: dlq
```

**DLQ writes are session-fenced.** Before writing an entry, the DLQ router checks the lease for the session that owns the failing route. The check is scoped to that session: a route with no exclusive session (empty session ID, or an ingress failure with no owning session) is never blocked, so a standby can DLQ its own unfenced ingress failures, while an unrelated instance's lease can no longer authorize a write for a route it does not own. The write is confirmed durable before the source delivery or outbox record is settled -- if the DLQ write fails, the record is left claimed for redelivery rather than lost. The gate is best-effort (the lease token is not passed to the store write), so a lease lost mid-write can produce a duplicate entry, never a lost one.

**Conservation law.** Every received message ends in exactly one terminal state, and the runtime metrics account for all of them: `MessagesReceived = MessagesSent + MessagesDropped + MessagesFiltered + MessagesExpired + DLQEntries + in-flight`. A rising `MessagesDropped` (terminated with neither a send nor a DLQ record) is the single signal for silent loss. `MessagesExpired` covers TTL drops (route-expired ingress and the drainer's expire sweep); `MessagesFiltered` covers deliberate processor discards. Watch these series together to prove the outbox is losing nothing.

### Strongest Guarantee: Keep `outbox_persist`

For `shared_outbox` the strongest source guarantee IS `outbox_persist` -- the
source is ACKed only after the message is durable in the outbox, so a crash never
loses it. There is no "wait for the target" variant on this delivery mode:
`ack_after: target_accept` is rejected at startup (`runtime/validator.go:278-286`),
because the drainer delivers to the target asynchronously and the source ACK cannot
be deferred that far.

If your requirement is that the source stay unacknowledged until the *target*
confirms delivery -- for example financial or regulatory data where you accept
coupling ingress throughput to egress latency -- use `direct_hold` instead of
`shared_outbox`:

```yaml
routes:
  - id: sensor-ingest
    receiver_id: mqtt-in
    delivery_mode: direct_hold
    bindings: [to-sqs]
    policy:
      ack_after: target_accept   # direct_hold default; the source is held open until the target accepts
      max_in_flight: 50
```

`direct_hold` trades the outbox's crash durability (a process death mid-send loses
the in-flight message) for end-to-end confirmation before ACK. Pick one boundary:
outbox durability with `shared_outbox`, or target confirmation with `direct_hold`.

### Combined: Durable Fan-Out

Combine `shared_outbox` with `fan_out` dispatch. Each binding gets its own outbox record, and the drainer delivers independently:

```yaml
routes:
  - id: durable-fanout
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    dispatch_mode: fan_out
    bindings: [to-sqs, to-archive]
    policy:
      ack_after: outbox_persist
      max_outbox_depth: 5000
```
