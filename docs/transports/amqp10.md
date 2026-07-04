# AMQP 1.0

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `amqp10`
**Factory:** `amqp10.NewFactory(logger)`
**Capabilities:** `stateful_session`, `source_redelivery`

The AMQP 1.0 adapter works with any broker that speaks the AMQP 1.0 wire
protocol: Apache ActiveMQ Artemis, Solace PubSub+, Apache Qpid, Azure
Event Hubs (direct AMQP), and AWS MQ for ActiveMQ. Built on
[github.com/Azure/go-amqp](https://github.com/Azure/go-amqp).

A `Session` owns the TCP connection and AMQP session. Receivers and senders
create links on that session. Topology (queues, topics) is broker-managed --
AMQP 1.0 does not declare exchanges or queues. The `options:` block is nested:
session settings under `options.session`, receiver settings under
`options.receiver`, sender settings under `options.sender`.

## YAML Example

```yaml
sessions:
  - id: artemis-conn
    transport: amqp10
    options:
      session:
        address: "amqp://localhost:5672"
        container_id: "bridge-node-01"
        connect_timeout: "30s"
        reconnect_delay: "1s"
        reconnect_max_delay: "30s"
        reconnect_multiplier: 2.0
        idle_timeout: "2m"
        max_frame_size: 65536
        sasl_mechanism: "plain"
        username: "admin"
        password: "admin"

receivers:
  - id: order-receiver
    transport: amqp10
    session_id: artemis-conn
    options:
      receiver:
        address: "queue://orders"
        link_credit: 20
        durability_mode: 0

senders:
  - id: event-publisher
    transport: amqp10
    session_id: artemis-conn
    options:
      sender:
        address: "topic://events"
        timeout: "30s"
        durability_mode: 0
        durable: true
```

## Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | SINGLE broker URL to dial, e.g. `amqp://host:5672` or `amqps://host:5671`. No client-side broker list / failover -- resolve to a load balancer or DNS for HA. |
| `container_id` | string | generated `gobridge-<16 hex chars>` | AMQP container ID identifying this client. When omitted, generated **once per session** with `crypto/rand` entropy — unique per replica and stable across reconnects, but NOT across process restarts. Durable subscriptions (`durability_mode > 0`) are keyed by container-id + link name, so they still require an explicit, per-replica-unique `container_id`. |
| `connect_timeout` | duration | `30s` | Dial timeout per connection attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect |
| `reconnect_max_delay` | duration | `30s` | Reconnect backoff ceiling |
| `reconnect_multiplier` | float | `2.0` | Reconnect backoff growth factor |
| `idle_timeout` | duration | `2m` | Connection idle timeout sent to broker |
| `max_frame_size` | int | `65536` | Maximum AMQP frame size in bytes |
| `link_close_timeout` | duration | `5s` | Deadline for closing a link/session during cleanup |
| `connection_monitor_fallback` | duration | `30s` | Fallback liveness re-check cadence (real disconnects use `Conn.Done()` immediately) |
| `sasl_mechanism` | string | `""` | `""` (PLAIN when `username` set, else no SASL), `plain`, `external` (mTLS client cert), or `anonymous` |
| `username` | string | -- | SASL PLAIN username |
| `password` | string | -- | SASL PLAIN password |
| `tls.enable` | bool | `false` | Enable TLS |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.ca_cert_pem` | string | -- | CA certificate PEM material (in-memory; non-empty wins over `ca_cert_file`). Secret-typed, redacted on marshal. Typically supplied by credential rotation. |
| `tls.cert_pem` | string | -- | Client certificate PEM material (non-empty wins over `cert_file`; requires `key_pem`). Secret-typed, redacted on marshal. |
| `tls.key_pem` | string | -- | Client private key PEM material (requires `cert_pem`). Secret-typed, redacted on marshal. |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

A top-level `credentials_uri` key sits directly under `options:` (a sibling of
`session`, not inside it) and is resolved by the bridge credential store at
build time:

```yaml
options:
  credentials_uri: "aws-secrets://gobridge/artemis"
  session:
    address: "amqp://localhost:5672"
```

## Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Queue or topic address to consume from |
| `link_credit` | int | `10` | Prefetch credit; how many messages the broker sends ahead |
| `durability_mode` | int | `0` | AMQP terminus durability for the receiver link (`0` none, `1` configuration, `2` unsettled-state). `> 0` makes the subscription **durable**. |
| `subscription_name` | string | -- | Pins the AMQP link name so a durable subscription survives reconnects. When empty and `durability_mode > 0`, a stable name is derived from `container_id` + `address`. |
| `routing` | string | `anycast` | `anycast` or `multicast` (Artemis routing type; case-insensitive). The typed decoder also accepts the legacy numeric forms `0` (anycast) and `1` (multicast). |

> **Durable subscription identity.** A durable subscription
> (`durability_mode > 0`) is identified by **container-id + link name**. A
> stable link name is required for the broker to *resume* an existing
> subscription rather than orphaning it and creating a new one on every
> reconnect -- set `subscription_name` (or rely on the derived
> `container_id`+`address` name) and keep `container_id` stable and unique per
> replica.

## Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Target address to publish to |
| `timeout` | duration | `30s` | Send timeout per message |
| `durability_mode` | int | `0` | AMQP terminus durability for the sender link |
| `durable` | bool | `true` | Sets the AMQP message `durable` header on outbound messages. **Unset defaults to `true` (persistent)** -- set `false` to opt into non-persistent (faster, lost on broker restart) sends. |
| `routing` | string | `anycast` | `anycast` or `multicast` (Artemis routing type; case-insensitive). The typed decoder also accepts the legacy numeric forms `0` (anycast) and `1` (multicast). |

## Settlement Mapping

| `ports.Delivery` method | AMQP 1.0 disposition | Notes |
|---|---|---|
| `Ack(ctx)` | `AcceptMessage` (accepted) | Message removed from queue |
| `Retry(ctx, 0, err)` | `ReleaseMessage` (released) | Immediate redelivery to any consumer |
| `Retry(ctx, >0, err)` | `ModifyMessage` (delivery-failed=true) | Signals broker to schedule retry |
| `Extend(ctx, deadline)` | -- | Returns `ErrNotSupported` |

Settlement is idempotent. Only the first call on a `Delivery` performs the
disposition; subsequent calls are no-ops.

## AMQP 1.0 vs AMQP 0-9-1

| Aspect | AMQP 1.0 | AMQP 0-9-1 (RabbitMQ) |
|--------|----------|----------------------|
| Topology | Broker-managed (no declare) | Client declares exchanges, queues, bindings |
| Retry with delay | `ModifyMessage` (broker-dependent) | Nack+requeue only (no native delay) |
| Flow control | Link credit (prefetch) | Channel-level QoS prefetch |
| Batch send | Individual messages over link | Individual messages with publisher confirms |
| Session model | Connection + AMQP session + links | Connection + channels |

## Resilience Behavior

- **Automatic reconnection.** On connection loss or link detach, the
  session reconnects with exponential backoff (1s initial, 30s cap, 25%
  jitter). Links re-create themselves on the next operation.
- **Link lifecycle.** Receivers and senders detect link errors and notify
  the session, which triggers reconnection. After reconnect, `Reconcile`
  runs again if a plan exists.
- **Idempotent settlement.** A `Delivery` can only be settled once. Repeat
  calls to `Ack` or `Retry` are safe no-ops.
- **Delayed retry is broker-controlled.** A `Retry(ctx, after>0, err)` cannot
  hold the message client-side. The adapter settles with `ModifyMessage`
  (`DeliveryFailed=true`) and attaches the `x-opt-delivery-time` message
  annotation (absolute ms-epoch) asking the broker to schedule redelivery; the
  broker owns the timing. Brokers that ignore the annotation redeliver
  immediately -- the deferral is counted on `AMQP10DelayedRetryUnhonored` and
  warned once per link. Such brokers **require a broker-side redelivery
  delay**: ActiveMQ Artemis defaults `redelivery-delay` to `0`, so without
  address-settings configuring `redelivery-delay` (and ideally
  `redelivery-delay-multiplier` / `max-redelivery-delay`) every delayed retry
  burns through `max-delivery-attempts` in milliseconds. Alert on a climbing
  `AMQP10DelayedRetryUnhonored`.

## AWS MQ (Managed AMQP 1.0)

AWS MQ for ActiveMQ supports the AMQP 1.0 protocol. Connect using the
broker's AMQP endpoint with TLS:

```yaml
sessions:
  - id: aws-mq-conn
    transport: amqp10
    options:
      session:
        address: "amqps://b-xxxx-xxxx.mq.eu-west-1.amazonaws.com:5671"
        container_id: "bridge-aws-mq"
        username: "admin"
        password: "your-password"
        tls:
          enable: true
```

AWS MQ manages broker topology (queues, topics) through the ActiveMQ admin
console or API. The bridge connects as a standard AMQP 1.0 client.

---

