# RabbitMQ (AMQP 0-9-1)

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `amqp091`
**Factory:** `amqp091.NewFactory(logger)`
**Capabilities:** `stateful_session`, `source_redelivery`, `plan_driven_subscriptions`

> amqp091 also latches `exclusive_identity` once it builds an exclusive consumer,
> so that capability is absent from the static list above until first exclusive
> use. Like the other session transports it is **single-use** -- an explicit
> `Close` is permanent (Start-after-Close returns `ErrUnavailable`), and the
> automatic reconnection described below recovers only from transient connection
> drops, not from an explicit `Close`. See
> [Exclusive transports are single-use](../transport-configuration.md#transport-capabilities-matrix).

RabbitMQ uses a stateful session. A `Session` owns a single AMQP connection
with automatic reconnection. Receivers and senders each open their own
channel on that connection. `Reconcile` declares exchanges, queues, and
bindings from the `SessionPlan`. The `options:` block is nested: session
settings under `options.session`, receiver settings under `options.receiver`,
sender settings under `options.sender`, per-topic declarations under
`options.subscription`, and publisher topology under `options.publisher`.

> **Plan-driven subscriptions.** amqp091 receivers subscribe (queue-declare +
> bind + consume) *only* when the session manager reconciles the `SessionPlan`.
> A receiver on an unmanaged session is silently inert, so the builder requires
> a session manager for amqp091 receivers.

## YAML Example

```yaml
sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      session:
        broker_url: "amqp://guest:guest@localhost:5672/"
        heartbeat: "10s"
        connect_timeout: "30s"
        reconnect_delay: "1s"
        reconnect_max_delay: "30s"
        reconnect_multiplier: 2.0
        tls:
          enable: false

receivers:
  - id: order-consumer
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "order-queue"
        options:
          subscription:
            exchange: "orders"
            exchange_type: "direct"
            routing_key: "new-order"
            durable: true
    options:
      receiver:
        queue_name: "order-queue"
        prefetch_count: 20

senders:
  - id: notification-publisher
    transport: amqp091
    session_id: rabbit-conn
    options:
      sender:
        exchange: "notifications"
        routing_key: "order.confirmed"
        delivery_mode: "persistent"
        mandatory: true
        timeout: "30s"
```

## Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- (required) | AMQP URI, e.g. `amqp://user:pass@host:5672/vhost` |
| `username` | string | -- | Explicit credential -- **overrides** any userinfo in `broker_url` (precedence note below) |
| `password` | string | -- | Explicit credential -- overrides `broker_url` userinfo |
| `vhost` | string | -- | AMQP virtual host |
| `heartbeat` | duration | `10s` | Connection heartbeat interval |
| `connect_timeout` | duration | `30s` | Dial timeout per attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect |
| `reconnect_max_delay` | duration | `30s` | Reconnect backoff ceiling |
| `reconnect_multiplier` | float | `2.0` | Reconnect backoff growth factor |
| `tls.enable` | bool | `false` | Enable TLS. The session dials with `amqp.DialConfig` and a `TLSClientConfig` assembled from the PEM material below; there is no `amqp091.DialTLS` call. |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

> **Duration keys need a unit.** Every duration option must be written as a
> string with a unit (`"30s"`, `"500ms"`). A bare number decodes as
> **nanoseconds** when the target is `time.Duration` (`heartbeat: 30` = 30 ns)
> and is rejected by the strict decoder; `Config.Validate` additionally rejects
> any non-zero duration below 1 ms as a decode accident. Zero means "use the
> default".

> **Credential precedence.** An explicitly configured `username`/`password`
> (or credential-store material) **overrides** userinfo embedded in
> `broker_url`. A URL carrying stale userinfo paired with an explicit
> `username` connects as the explicit credential, so a rotated secret is not
> silently defeated by stale URL userinfo. When both are present the embedded
> userinfo is dead config: the session logs a **Warn** at construction and on
> each rotation (with the URL redacted). Keep `broker_url` credential-free.

## Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Queue to consume from |
| `consumer_tag` | string | auto-generated | Consumer tag identifier |
| `exclusive` | bool | `false` | Exclusive consumer |
| `prefetch_count` | int | `10` | QoS prefetch count. `0` maps to the bounded default (10), **not** unlimited -- an unbounded window lets the broker hand the whole queue to one consumer. Set an explicit positive value for a larger window. |
| `prefetch_size` | int | `0` | QoS prefetch size in bytes |

> **Removed:** `auto_ack` is rejected (`auto_ack: true` fails validation --
> broker auto-ack acks on delivery and silently drops messages when a
> downstream step fails; the bridge settles only after the send/persist
> succeeds).

## Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `exchange` | string | `""` | Target exchange name |
| `routing_key` | string | `""` | Static fallback routing key. The per-dispatch `OutboundMessage.Address` (resolved from the dispatch plan) **wins**; `routing_key` applies only when `Address` is empty. If both are empty the publish is rejected with `ErrInvalidTopic`. The routing key is **never** taken from `Envelope.Subject` — Subject travels as a header, not as a transport address. |
| `delivery_mode` | string | `persistent` | `persistent` (AMQP delivery-mode 2, survives a broker restart on a durable queue) or `transient` (delivery-mode 1, lost on restart). A per-message `amqp091.delivery-mode` header overrides it (accepts `1`/`2` or `transient`/`persistent`; any other value is ignored and the configured default applies); quorum queues persist regardless. Empty resolves to `persistent`; an invalid value is rejected at config validation (`config.go` `validateDeliveryMode`). |
| `mandatory` | bool | `false` | Return an unroutable publish as a `basic.return` instead of letting the broker confirm-then-discard it. See the unroutable-publish note below — the managed factory requires `mandatory: true` or `allow_unroutable_drop: true`. |
| `allow_unroutable_drop` | bool | `false` | Explicit opt-in that lets a managed sender publish with `mandatory: false`, deliberately accepting the silent broker discard of unroutable publishes (throughput-over-safety fan-out where unroutable is expected). It never changes the publish; it only records that the operator accepts the loss. |
| `timeout` | duration | `30s` | Per-publish timeout (applied when context has no deadline) |

> **Unroutable publishes and `mandatory`.** With `mandatory: false` (the decode
> default) RabbitMQ confirms an unroutable publish — wrong routing key or missing
> binding — then discards it, so `Send` succeeds, the source is acked, and the
> message is lost with no telemetry. The managed factory therefore refuses to
> build a sender unless it sets `mandatory: true` or `allow_unroutable_drop: true`
> (`factory.go`). With `mandatory: true` an unroutable publish comes back as a
> `basic.return`; the sender surfaces it as a permanent `ErrNotFound`
> ("mandatory publish unroutable") so the route retries or DLQs it instead of
> acking the source (`sender.go`). Batched sends fall back to sequential `Send`
> when `mandatory` is set, because `basic.return` carries no delivery tag to
> attribute the bounce to a specific message.

> **Removed:** `immediate` is rejected (`immediate: true` fails validation --
> RabbitMQ removed `basic.publish` `immediate` in 3.0 and closes the channel
> when it is set).

## Topology Declaration (Reconcile)

AMQP 0-9-1 sessions support automatic topology declaration via `Reconcile`.
When a receiver has `topics[]` entries with `options`, the session declares
the exchange, queue, and binding during startup and after each reconnect.

```yaml
topics:
  - topic: "my-queue"
    options:
      subscription:
        exchange: "my-exchange"
        exchange_type: "direct"
        routing_key: "events"
        durable: true
```

This declares:

1. Exchange `my-exchange` of type `direct` (durable)
2. Queue `my-queue` (durable)
3. Binding from `my-exchange` to `my-queue` with routing key `events`

### Publisher-side exchange auto-declare (best-effort)

A sender's target exchange is also pre-declared during `Reconcile`, so a route
that publishes to an exchange the bridge owns works without a separate
provisioning step:

```yaml
senders:
  - id: notification-publisher
    transport: amqp091
    session_id: rabbit-conn
    options:
      sender:
        exchange: "notifications"      # names the exchange to declare + publish to
        routing_key: "order.confirmed"
      publisher:                       # optional: topology for that exchange
        exchange_type: "topic"
        durable: true
```

Two rules govern the declaration:

- **`sender.exchange` names the exchange; the `publisher.*` block only
  decorates it.** The declared name is always `sender.exchange` (the exact
  exchange `Send` publishes to). `exchange_type`, `durable`, `auto_delete`, and
  `exchange_arguments` come from the separate `publisher.*` block. With
  `sender.exchange` set and no `publisher.*` block, the exchange is declared with
  defaults (`direct`, non-durable). `publisher.exchange` is **not** a second name
  and is ignored by the declare.
- **The declare is best-effort — it never takes a route down.** If the broker
  rejects it (`PRECONDITION_FAILED` when the exchange already exists with
  different topology, or `ACCESS_REFUSED` under least-privilege credentials that
  lack `configure`), the session logs a warning, increments
  `AMQP091PublisherDeclareFailed`, and continues. Publishing still works when the
  exchange already exists; a genuinely-absent exchange the bridge cannot create
  instead fails visibly at publish time (404 → retry/DLQ). This makes it safe to
  point a sender at an externally-managed exchange without pre-declaring a
  matching `publisher.*` topology.

## Settlement Mapping

| `ports.Delivery` method | AMQP 0-9-1 operation | Notes |
|---|---|---|
| `Ack(ctx)` | `delivery.Ack(false)` | Single-message acknowledgement |
| `Retry(ctx, after, err)` | `delivery.Nack(false, true)` | Requeue; `after` is logged but not enforced |
| `Extend(ctx, deadline)` | -- | Returns `ErrNotSupported` |

## Header Mapping

The adapter maps AMQP 0-9-1 per-message properties to and from the envelope
end-to-end. Inbound properties become envelope fields and headers; outbound
`amqp091.*` headers set the matching AMQP publish properties.

### Inbound TTL becomes an absolute deadline

An inbound `Expiration` (the AMQP 0-9-1 per-message expiration -- a short string
of whole milliseconds) maps to an **absolute** `ExpiresAt` on the envelope,
anchored at receipt time. The TTL travels as a deadline, not a relative
countdown, so it no longer restarts at each hop: egress re-derives the remaining
relative TTL from `ExpiresAt` (`Envelope.RemainingTTL`), and the remaining budget
shrinks by the in-bridge dwell.

> **Behavior change -- an expiring message can now be dispositioned in-bridge.**
> A message whose in-bridge dwell (outbox persistence plus retry backoff) exceeds
> its remaining TTL is expiry-dispositioned instead of flowing through
> transparently as it did before. Under the route's `on_expired: dlq` it is
> DLQ'd; under `on_expired: drop` it is dropped and counted (`MessagesExpired`).
> The disposition is correct and always observable -- never silent. Size the
> outbox `retention` and any upstream redelivery window against the shortest
> producer TTL you carry. See [route delivery
> policy](../routes-and-runtime-reference.md).

An over-range `Expiration` -- greater than ~9.2e12 ms (~292 years), the value a
producer uses as an "effectively never expire" sentinel -- maps to **no** expiry,
as do empty, non-numeric, and negative values. The mapping fails toward delivery:
an unmappable TTL never becomes a past deadline that would drop the message on
arrival.

### Egress property coercion (priority, timestamp)

Outbound header mapping coerces numeric types instead of requiring an exact Go
type, so an override supplied from a YAML/JSON route config or produced by another
transport is honored instead of silently dropped (an earlier build asserted the
exact type and dropped everything else -- `priority: 9` in YAML published at `0`).

| Header | Accepts | Applied when | Ignored (property left unset) when |
|---|---|---|---|
| `amqp091.priority` | any Go integer (`int`, `int8`–`int64`, `uint8`–`uint64`), a non-fractional `float32`/`float64`, or a numeric string (e.g. `9` as an int) | in range `0..255` (the AMQP priority octet; the AMQP 0-9-1 base spec uses `0..9`, RabbitMQ priority queues extend to 255) | out of range, non-integral, or a native `uint` -- broker default `0` applies |
| `amqp091.timestamp` | POSIX seconds as `int`, `int32`, `int64`, `uint32`, `uint64`, or `float64`; a Go `time.Time`; or an RFC3339 string | it parses to a valid epoch time | any other type, unparseable, `NaN`/`Inf`, or outside the `int64`-seconds range |

An out-of-range or unparseable value leaves the property unset instead of
publishing a wrong one: a bad `priority` falls back to the broker default and a
bad `timestamp` is omitted, never emitted as a garbage pre-epoch value.

## Resilience Behavior

- **Automatic reconnection.** On connection loss, the session retries with
  exponential backoff (1s initial, 30s cap) plus 25% jitter. After
  reconnecting, it re-runs `Reconcile` to redeclare topology.
- **Publisher confirms.** The sender opens its channel in confirm mode.
  Each `Send` waits for the broker to confirm receipt before returning.
- **Channel isolation.** Each receiver and sender opens its own channel.
  A channel-level error (e.g., queue deleted) does not affect other
  receivers and senders on the same connection.

### Flow control (`BROKER_BUSY`)

When RabbitMQ raises a resource alarm (memory or disk watermark) it stops
reading from publisher connections and sends a `connection.blocked` notification.
The session tracks that state; while it is engaged the sender **fails fast**: a
`Send` returns `shared.ErrBrokerBusy` (code `BROKER_BUSY`, transient) with the
broker-supplied reason instead of issuing the publish.

The fail-fast is deliberate. The amqp091-go call
`PublishWithDeferredConfirmWithContext` ignores the context once the broker is
not draining the socket, so a publish issued during an alarm blocks
indefinitely -- and it blocks while holding the session mutex, wedging every
other sender on the connection past its own deadline. Refusing up front converts
an unbounded stall into a prompt, retryable error the runtime can back off on.
`BROKER_BUSY` clears on its own when the broker lifts the alarm; treat it as
backpressure, not a failure. See [Troubleshooting](../troubleshooting.md) for
the alarm-clearing checklist.

### Publish timeouts and bounded at-least-once

A publish is bounded by the sender's configured timeout even though the
amqp091-go deferred-publish call ignores the context. When a publish (or its
publisher confirm) exceeds the deadline on a half-dead broker, `Send` returns a
transient error (`SERVICE_UNAVAILABLE` on cancel, `TIMEOUT` on deadline) and
**abandons** the wedged channel to a background reaper instead of closing it
under the sender mutex — a synchronous close would itself block on the same
stalled broker and wedge every other publish, shutdown, and reconfiguration.
The number of concurrently-abandoned channels is hard-capped per sender
(default 8, internal — not operator-tunable): once saturated the sender fails
fast with `BROKER_BUSY` until the wedged publishes drain (broker recovery or
reconnect), so a black-holed connection cannot pile up unbounded reaper
goroutines.

`SendBatch` (non-mandatory) pipelines: it publishes every message, then awaits
the confirms. This is **at-least-once with a bounded duplicate window**. A
confirm that has already arrived is always honored with its real outcome, even
if the batch deadline has since expired. But if the deadline fires while a
publish is genuinely mid-flight, that message **and the still-unconfirmed prefix
before it** are reported transient so the caller retries them — and a retry may
duplicate any of that prefix the broker had in fact already accepted. The
duplicate ceiling is the number of unsettled in-flight confirms at the instant
the deadline fires. Consumers must be idempotent (this is inherent to
at-least-once publishing under a hard deadline, not specific to this adapter).

### Credential rotation and sustained outages

`ApplyCredentials` rotates username/password and/or TLS material by
**close-then-redial** (AMQP 0-9-1 has no re-auth primitive). When the caller's
context is cancelled or deadlines mid-rotation, the adapter does **not** wait for
the live connection's close to finish: it detaches the stale connection under
the lock (drops it from the session, marks the session disconnected) and
explicitly wakes the reconnect loop. A sender that grabs the connection after a
rotation therefore can never publish on the old connection with the old
credentials, and reconnect proceeds even if the old connection's close wedges on
a half-dead broker.

Every teardown path is bounded so a sustained outage cannot leak goroutines:
topology declaration on start/reconnect is deadline-bounded (`ConnectTimeout`),
each discarded connection is closed under a deadline (the dial timeout), and the
number of concurrent close goroutines is capped — beyond the cap a connection is
dropped without an explicit close (the OS reaps the socket and the broker reaps
the peer when heartbeats stop), which is strictly better than an unbounded
goroutine pile-up.

---

