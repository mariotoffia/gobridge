# AMQP 1.0

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `amqp10`
**Factory:** `amqp10.NewFactory(logger)`
**Capabilities:** `stateful_session`, `source_redelivery`

The AMQP 1.0 adapter is built and CI-tested against Apache ActiveMQ Artemis. It
attaches links with Artemis-style `queue`/`topic` terminus capabilities on every
attach, so a broker that reads those capabilities differently — Apache Qpid,
Solace PubSub+, Azure Event Hubs (direct AMQP), AWS MQ for ActiveMQ — may reject
the attach or route messages differently. Treat any non-Artemis broker as
unverified and test it before relying on it. Built on
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
        allow_insecure_plain: true  # local/dev only: PLAIN over amqp:// is cleartext; use amqps:// in production

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
| `container_id` | string | generated `gobridge-<16 hex chars>` | AMQP container ID identifying this client. When omitted, generated **once per session** with `crypto/rand` entropy — unique per replica and stable across reconnects, but NOT across process restarts. Durable subscriptions (`durability_mode > 0`) are keyed by container-id + link name, so they **require an explicit, per-replica-unique `container_id`**: building a durable receiver without one is **rejected at build time** (a generated id changes on restart and orphans the subscription). |
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
| `allow_insecure_plain` | bool | `false` | Opt-in to send SASL PLAIN credentials over a non-TLS scheme. By default a PLAIN mechanism (explicit `sasl_mechanism: plain`, the inferred default when a username is set, or userinfo in `address`) over `amqp://` (or a schemeless address) is **rejected** at config validation, because PLAIN transmits the username/password in cleartext. Prefer `amqps://` / `amqp+ssl://`, or `sasl_mechanism: external` for mTLS; set this only to send credentials in the clear on a trusted network. |
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

When `sasl_mechanism: external`, the client certificate material may be
supplied on the transport config (`tls.cert_pem`/`tls.key_pem` in-memory, or
`tls.cert_file`/`tls.key_file` file paths) or resolved from `credentials_uri`.
If neither provides it, config validation fails fast with a clear error rather
than an opaque broker SASL failure at dial.

> **SASL PLAIN is rejected over a non-TLS scheme (secure by default).** PLAIN
> sends the username/password in cleartext frames, so config validation refuses it
> over `amqp://` (or a schemeless address) unless `allow_insecure_plain: true` is
> set. The gate holds on every credential path — construction, build-time
> credential resolution, and runtime rotation — so a rotation that would newly
> expose PLAIN over plaintext is refused and the session keeps its last-good
> credentials (no cleartext re-dial). Use `amqps://` / `amqp+ssl://`, switch to
> `sasl_mechanism: external` (mTLS), or opt in explicitly with
> `allow_insecure_plain: true` as a deliberate, auditable choice.

## Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Queue or topic address to consume from |
| `link_credit` | int | `10` | Prefetch credit; how many messages the broker sends ahead |
| `durability_mode` | int | `0` | AMQP terminus durability for the receiver link (`0` none, `1` configuration, `2` unsettled-state). `> 0` makes the subscription **durable**. |
| `subscription_name` | string | -- | Pins the AMQP link name so a durable subscription survives reconnects. When empty and `durability_mode > 0`, a stable name is derived from `container_id` + `address`. |
| `routing` | string | `anycast` | `anycast` or `multicast` (Artemis routing type; case-insensitive). The typed decoder also accepts the legacy numeric forms `0` (anycast) and `1` (multicast). |

> **Durable subscription identity (enforced).** A durable subscription
> (`durability_mode > 0`) is identified by **container-id + link name**. A
> stable link name is required for the broker to *resume* an existing
> subscription rather than orphaning it and creating a new one on every
> reconnect -- set `subscription_name` (or rely on the derived
> `container_id`+`address` name) and keep `container_id` stable and unique per
> replica. Because a generated `container_id` changes on process restart, a
> durable receiver built **without an explicit `session.container_id` is
> rejected at build time** (fail-closed): durable mode that silently loses
> continuity across a restart is worse than a startup error.

> **Multicast fans out duplicates.** `routing: multicast` uses pub-sub (topic)
> semantics: the broker copies each message to **every** active subscription at
> send time, so N subscribers each receive their own copy. `routing: anycast`
> (the default) is point-to-point -- exactly one receiver consumes each message.
> Under multicast a subscriber only receives messages sent **while it is
> attached** unless the subscription is durable (`durability_mode > 0`) and
> resumes under a stable container-id + link name. A detached non-durable
> subscription -- or a durable one orphaned by an unstable link name -- silently
> misses everything sent in the gap.

> **Durable receivers need a dedicated session (enforced).** Closing a durable
> receiver (`durability_mode > 0`) cannot use a normal link detach: the pinned
> go-amqp can only send a closing detach, which Artemis reads as UNSUBSCRIBE and
> destroys the durable subscription (dropping every retained message). To detach
> the live link while preserving the subscription the adapter drops the whole
> connection — a non-closing detach of **every** link on that session. So a
> durable receiver sharing a session with other receivers or senders would blip
> all of them on close: in-flight sender publishes relatch and non-durable
> receivers redeliver. To keep that blast radius off unrelated traffic, the
> factory **rejects at build time** any config that multiplexes a durable
> receiver with another link on the same session — give each durable (multicast)
> receiver its own `session_id`.

> **No safe clustered durable multicast.** Two replicas that resume the same
> durable multicast identity (container-id + link name) fight over the link: the
> broker grants it to one and detaches the other permanently with `amqp:link:stolen`
> (classified permanent — retrying just re-steals it back forever). Giving each
> replica a unique identity avoids the theft, but then every replica keeps its own
> copy of each message (one copy per replica), not a shared load-balanced stream.
> For competing consumers across a cluster use `anycast` (point-to-point), which
> the broker load-balances across attached receivers.

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

Settlement is single-shot. The first `Ack` or `Retry` on a `Delivery` performs
the disposition and records the outcome. A later call is a safe no-op (returns
`nil`) **only** when that first attempt succeeded; a repeat call while the first
is still in flight returns `ErrUnavailable` (`amqp10: delivery settlement
already in progress`), and a repeat call after the first attempt **failed**
returns `ErrUnavailable` (`amqp10: delivery settlement previously failed`). A
failed settlement is surfaced, not silently swallowed.

## Envelope identity: publish a `message-id`

A producer's `message-id` property becomes `Envelope.ID`. A message without one
gets an identity minted at ingress, so the same message released or modified
back to the broker is converted under a **new** identity on redelivery. The
adapter marks such an envelope `x-bridge.generated-id` to say so.

A message that carries a header section is still countable through its
`delivery-count`, so it keeps the route's full `max_replay_attempts` budget. A
message with neither a `message-id` nor a header section is uncountable: the
runtime settles its first transient failure terminally (DLQ, or dropped per the
route's `on_permanent_failure`) rather than redelivering on an identity that can
never accumulate attempts.

## AMQP 1.0 vs AMQP 0-9-1

| Aspect | AMQP 1.0 | AMQP 0-9-1 (RabbitMQ) |
|--------|----------|----------------------|
| Topology | Broker-managed (no declare) | Client declares exchanges, queues, bindings |
| Retry with delay | `ModifyMessage` (broker-dependent) | Nack+requeue only (no native delay) |
| Flow control | Link credit (prefetch) | Channel-level QoS prefetch |
| Batch send | Individual messages over link | Individual messages with publisher confirms |
| Session model | Connection + AMQP session + links | Connection + channels |

## Resilience Behavior

- **Recovery has three scopes.** A fault is recovered at the narrowest scope that
  covers it, so a single link problem does not disrupt the whole connection.
  - *Connection-scoped* (connection lost, unclassified unavailable): the session
    reconnects with exponential backoff (1s initial, 30s cap, 25% jitter),
    rebuilding the connection and every link on it. After reconnect `Reconcile`
    runs again if a plan exists.
  - *Receiver link-scoped* (an `*amqp.LinkError` on a live session): only that
    receiver's link is rebuilt; the shared connection and its sibling links stay
    up.
  - *Sender link-scoped*: the sender abandons the failed link and rebuilds it
    lazily on the next `Send` — there is no background sender reconnect, so a
    sender that never sends again never notices its link failed.
- **Send timeout leaves the outcome unknown.** A send waits for the broker's
  disposition (`SendWithReceipt` then `receipt.Wait`). If `timeout` (or the
  context deadline) fires during the wait, the adapter returns an error while the
  broker may already hold the message, so the runtime retries and the sink can see
  a duplicate. Use idempotent sinks (or downstream dedup) on this path.
- **Single-shot settlement.** A `Delivery` settles once. A repeat `Ack` or
  `Retry` is a safe no-op **only** after a successful first settlement; a repeat
  after a failed or still-in-flight first attempt returns `ErrUnavailable`
  rather than silently succeeding (see [Settlement Mapping](#settlement-mapping)).
- **Delayed retry is broker-controlled.** A `Retry(ctx, after>0, err)` cannot
  hold the message client-side. The adapter settles with `ModifyMessage`
  (`DeliveryFailed=true`) and attaches the `x-opt-delivery-time` message
  annotation (absolute ms-epoch) asking the broker to schedule redelivery; the
  broker owns the timing. Every delayed retry deferred this way increments
  `AMQP10DelayedRetryDeferred` (once per message) and emits a Warn once per
  link. The counter measures broker-delegated retry scheduling, **not** a
  failure: on a broker that honors the annotation (Artemis) the requested
  spacing is applied; on one that ignores it the spacing falls back to the
  broker's own redelivery policy. Ignoring brokers therefore **require a
  broker-side redelivery delay**: ActiveMQ Artemis defaults `redelivery-delay`
  to `0`, so without address-settings configuring `redelivery-delay` (and
  ideally `redelivery-delay-multiplier` / `max-redelivery-delay`) every delayed
  retry burns through `max-delivery-attempts` in milliseconds. A climbing
  `AMQP10DelayedRetryDeferred` is expected under retry load -- correlate it with
  delivery-attempt exhaustion rather than treating the counter itself as an
  error signal.

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

