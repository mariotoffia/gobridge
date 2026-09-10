# Durable shared outbox — config walkthrough

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
| `target_accept` | ACK only after the target sender confirms delivery | **Rejected on a `shared_outbox` route** -- the runtime fails validation at startup (`runtime/validator.go`). It is the `direct_hold` default, where there is no outbox to persist to. |

With `outbox_persist`, the MQTT PUBACK is sent the moment the outbox store confirms
the write. The message is durable in the outbox but has not yet reached SQS. This
keeps ingress latency low and is the boundary `shared_outbox` is built around.

Setting `ack_after: target_accept` on a `shared_outbox` route is a startup error,
not a stronger guarantee -- the drainer sends to the target asynchronously, so the
source ACK can never be deferred to the target. If you need the source held open
until the target accepts, use `delivery_mode: direct_hold` instead; that gives up
per-destination progress and the buffer, not crash safety. Omitting
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
