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
| `delivery_mode` | string | `persistent` | `persistent` (AMQP delivery-mode 2, survives a broker restart on a durable queue) or `transient` (delivery-mode 1, lost on restart). A per-message `amqp091.delivery-mode` header overrides it; quorum queues persist regardless. Empty/invalid resolves to `persistent`. |
| `mandatory` | bool | `false` | Return unroutable messages |
| `timeout` | duration | `30s` | Per-publish timeout (applied when context has no deadline) |

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

---

