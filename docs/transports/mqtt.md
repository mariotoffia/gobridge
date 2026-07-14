# MQTT (Paho)

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `mqtt`
**Factory:** `paho.NewFactory(logger)`
**Capabilities:** `stateful_session`, `exclusive_identity`, `shared_consumer`, `plan_driven_subscriptions`

MQTT requires a session. Multiple receivers and senders can share one session
(one TCP connection). Session mode controls lifecycle and ownership semantics.

The `options:` block is decoded into the transport's nested typed config: session
connection settings live under an `options.session` sub-block and sender settings
under an `options.sender` sub-block. The only other key allowed directly under
`options:` is `credentials_uri`, which resolves broker credentials from a store.
Putting session or sender keys flat under `options:` is rejected by the strict
decoder.

Because MQTT advertises `plan_driven_subscriptions`, every MQTT receiver
subscribes only when the session manager reconciles the session plan. The bridge
builder therefore fails the build if an MQTT receiver is bound to a session that
never gets a manager (it would otherwise be silently inert, subscribing to
nothing).

## Session Modes

| Mode | `session_mode` | Effective clean-start on the wire | Behavior |
|------|---------------|-----------------------------------|----------|
| Ephemeral | `ephemeral` (default) | always `true` (the `clean_start` option is ignored) | No state survives disconnect |
| Persistent | `persistent` | honours `clean_start` (default `false`) | Broker retains subscriptions and queued messages |
| Exclusive | `exclusive` | always `false` (`clean_start: true` is overridden to `false` with a warning) | Lease-based single holder; requires a lease store |

The `clean_start` option defaults to **`false`** and is consulted only for
Persistent and Exclusive sessions — the modes that exist to *resume* broker
session state. Ephemeral sessions always connect with clean-start regardless of
the option. `clean_start: true` on an Exclusive session is a misconfiguration
(autopaho would reconnect with the same client ID and clean-start, producing a
session-takeover loop); the adapter overrides it to `false` and logs a warning
(`acl_session.go`).

### Exclusive mode: lease store and failover timing

**Lease store (platform requirement).** Exclusive mode elects a single holder
through a distributed lease, so it needs a lease store that every instance
shares. The only production-grade lease store today is **DynamoDB**
(`store.type: dynamodb`). The in-process `memory` lease store coordinates only
*within one process*: it is fine for a single-node deployment or tests, but it
**cannot** enforce single-owner exclusivity across a multi-node cluster.
A multi-node cluster running exclusive sessions therefore currently **requires
AWS DynamoDB** — a non-AWS multi-node cluster cannot run exclusive sessions
until a portable lease store (e.g. Postgres or Redis) is added. Single-node
exclusive sessions have no such coupling. See
[store configuration](../configuration-reference.md#stores----backing-store-configuration).

**Failover timing.** When the lease holder dies, a standby reclaims the
partition only after the lease TTL lapses, so the failover window is
approximately `lease_ttl`. A clustered exclusive route that does **not** pin
`lease_ttl`/`renew_interval` automatically starts from the 45s HA profile, which
lands in the documented 30–60s band; pinning a looser `lease_ttl` (>60s) makes
failover proportionally slower and emits a startup WARN so the trade-off is
deliberate. See
[Scenario 8 — High-Availability Profile](../scenarios/08-clustered-exclusive-sessions.md#high-availability-profile)
for the failover math and the invariants.

**Restart policy is a deployment requirement.** The Paho session is
single-use: once `Close` runs (on lease loss / step-down) it does not reconnect
in-process. Re-acquiring the lease therefore costs a **process restart**, driven
by the runtime going terminal (liveness fails closed and a non-zero-exit backstop
fires). A clustered exclusive deployment **must** run under a restart policy that
brings the process back: on Kubernetes the default `restartPolicy: Always`
suffices (a `livenessProbe` on `/api/v1/monitor/live` gives faster detection);
under systemd use `Restart=on-failure` (or `always`); under bare `docker run` use
`--restart unless-stopped`. Readiness alone is insufficient — it removes the pod
from the load balancer but does not restart a terminal runtime. See
[scenario 08 `connect_after_lease`](../scenarios/08-clustered-exclusive-sessions.md#connect_after_lease-true).

On a resumed (`clean_start=false`) session the broker replays its queued
backlog on CONNACK before the route runners have registered their topic
filters, so briefly some publishes match no handler. Those are buffered for
`unmatched_grace` (default 30s, restarted on every reconnect). After the
window a still-unmatched publish splits two ways. If a route the session
still wants covers the topic — its receiver handler only registered late —
the adapter **retains** the publish **un-acked** (it is never acked-and-dropped,
so at-least-once holds) and counts `MQTTRouterCoveredRetained`; the buffered
publish is delivered once the handler registers. The one exception is a covered
**QoS 0** publish the bounded pending buffer cannot hold: QoS 0 has no
redelivery contract, so it is dropped best-effort and counted
`MQTTRouterCoveredDropped` (QoS 1/2 are never dropped for a covered topic —
they are held instead). Otherwise the topic is an **orphan subscription** —
typically a route removed from config whose broker-side subscription survived
the resume — and the adapter acks-and-drops it, UNSUBSCRIBEs its exact topic,
and counts `MQTTRouterUnmatchedDropped` (benign cleanup), so the orphan no
longer stalls in-order acking for the rest of the shared session. See
[`paho/doc.go`](../../adapters/mqtt/transport/paho/doc.go)
for the full mechanism.

> **Migration / release note — covered-retention metric semantics.**
> `MQTTRouterCoveredDropped` now counts **only** covered **QoS 0** publishes the
> bounded pending buffer could not hold (QoS 0 has no redelivery contract). It no
> longer counts any QoS 1/2 loss, because a covered QoS 1/2 publish is **never**
> acked-dropped — it is retained un-acked and redelivered, counted on
> `MQTTRouterCoveredRetained`. Operators watching for a late- or never-registering
> **live** route (a receiver whose handler is slow to come up, or config that
> removed a still-subscribed route) must alert on **`MQTTRouterCoveredRetained`**
> (with the per-topic `RETAINED covered` WARN), not on `MQTTRouterCoveredDropped`.
> A sustained non-zero `MQTTRouterCoveredRetained` means a wanted topic's handler
> is not consuming and the receive-window is being pinned — investigate the route,
> not the buffer. `MQTTRouterUnmatchedDropped` remains orphan-cleanup only.

## YAML Example

```yaml
sessions:
  - id: mqtt-session-1
    transport: mqtt
    session_mode: persistent
    options:
      session:
        # Credentials are sent in the MQTT CONNECT packet in CLEARTEXT, so a
        # TLS scheme is required whenever username/password are set. autopaho
        # selects TLS from the URL SCHEME (ssl://, mqtts://, tls://, wss://),
        # NOT from tls.enable — a tcp:// URL stays cleartext even with
        # tls.enable=true, and the adapter refuses to ship credentials over it
        # unless allow_plaintext_credentials=true.
        broker_url: "ssl://broker.example.com:8883"
        client_id: "bridge-node-01"
        keep_alive: 30
        connect_timeout: "30s"
        reconnect_timeout: "10s"
        reconnect_delay: "5s"
        clean_start: false
        session_expiry_interval: 86400
        receive_maximum: 100
        username: "bridge"
        password: "secret"
        will:
          topic: "bridge/status/node-01"
          payload: "offline"
          qos: 1
          retain: true
        tls:
          enable: true
          ca_cert_file: "/etc/certs/ca.pem"
          cert_file: "/etc/certs/client.pem"
          key_file: "/etc/certs/client-key.pem"
          insecure_skip_verify: false

receivers:
  - id: sensor-receiver
    transport: mqtt
    session_id: mqtt-session-1
    topics:
      - topic: "sensors/+/temperature"
        qos: 1
      - topic: "sensors/+/humidity"
        qos: 1

senders:
  - id: command-sender
    transport: mqtt
    session_id: mqtt-session-1
    options:
      sender:
        default_topic: "devices/commands"
        qos: 1
        retain: false
        timeout: "30s"
        throttle_retry_after: "500ms"
```

## Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- | Single broker URL (e.g. `tcp://host:1883`). Folded into `broker_urls` when the list form is absent. |
| `broker_urls` | []string | -- | Multiple broker URLs for failover |
| `client_id` | string | -- | MQTT client identifier. **Required** on the effective (merged) session config at build time, together with at least one broker URL (`factory.go:59-66`, `NewSession`); an empty value is accepted at parse time. For scale-out uniqueness from one shared config file, see `client_id_suffix`. |
| `client_id_suffix` | string | -- | Opt-in per-instance uniquifier appended to `client_id` at build time, so one shared config file (ConfigMap, ECS task def) can still give every replica a distinct id. `hostname` appends `-<hostname>` (the pod/container name — deterministic and human-readable in logs); `nonce` appends `-<8 hex>` from `crypto/rand` (unique per process; use when the hostname is not distinct). Unset (default) leaves `client_id` verbatim. **Rejected on `session_mode: exclusive`** — exclusive failover requires a *stable shared* id across instances (see [scenario 08](../scenarios/08-clustered-exclusive-sessions.md)); build fails if set there. Use it only for `ephemeral`/`persistent` `$share` scale-out. |
| `keep_alive` | int | `30` | Keep-alive interval in seconds. Explicit `0` disables the MQTT pinger — half-open-connection detection then rests on TCP keep-alive alone (much slower, and OS-dependent), so a dead-but-open socket can go unnoticed for minutes. The registry/blueprint path defaults to `30`; a direct library consumer that sets `0` should understand this trade-off. |
| `connect_timeout` | duration | `30s` | Bounds the **initial** Start connection await |
| `reconnect_timeout` | duration | `30s` | Bounds each individual (re)connect attempt (TCP dial + TLS + CONNECT/CONNACK). Maps to autopaho `ConnectTimeout`; `0` → autopaho default (10s). |
| `reconcile_timeout` | duration | `30s` (`DefaultReconcileTimeout`) | Bounds **each** broker SUBSCRIBE / UNSUBSCRIBE issued while reconciling the session plan. The reconcile runs on a possibly deadline-less runtime context, so without this an unresponsive broker (SUBACK/UNSUBACK never arrives on a half-open connection) would hang the reconcile — and any startup / hot-reload step awaiting it — indefinitely. This is a **liveness safety bound**: a non-positive value is coerced **up** to `30s` and cannot be disabled. |
| `reconnect_delay` | duration | `10s` (`DefaultReconnectDelay`) | **Base** delay of a jittered exponential reconnect backoff. Starts at `reconnect_delay`, grows 2× (`reconnectBackoffFactor`) per failed attempt, caps at `reconnect_max_delay`, then equal-jitters to `[d/2, d)` to desynchronise a reconnecting fleet (anti thundering-herd). `0` → `10s`. |
| `reconnect_max_delay` | duration | `2m` (`DefaultReconnectMaxDelay`) | Caps the jittered-exponential reconnect envelope. Must be ≥ `reconnect_delay`; a smaller value is clamped up to the base at Start. `0` → `2m`. |
| `clean_start` | bool | `false` | MQTT 5 clean-start flag; consulted only for Persistent/Exclusive sessions |
| `session_expiry_interval` | int | `0` | MQTT 5 session expiry in seconds. For Persistent/Exclusive sessions a `0` is replaced at session creation (`NewSession`) with `86400` (24h) — a literal `0` would give zero offline retention. Ephemeral always uses `0`. |
| `receive_maximum` | int | `0` → **1024** (`DefaultReceiveMaximum`) | MQTT 5 Receive Maximum: max in-flight QoS 1/2 messages the broker may send before PUBACKs. `0` is not a legal MQTT v5 value, so an unset/`0` is coerced to **1024** to bound worst-case buffered memory — set it explicitly to `65535` for the old protocol ceiling. Bounds only the QoS 1/2 un-acked window — it does **not** throttle QoS 0. Also sizes the startup pending buffer used during `unmatched_grace`. |
| `max_payload_bytes` | int | `0` (unset) | Maximum application payload (message body) in bytes the session admits from the broker. When non-zero the adapter advertises an MQTT v5 Maximum Packet Size in CONNECT, derived from this value plus a ~128 KiB protocol-overhead allowance and clamped to the MQTT v5 four-byte ceiling (256 MiB − 1), so the broker must not deliver a larger packet. **The ~128 KiB is deliberate slack for MQTT properties/topic overhead, not a hard body cap:** a payload up to ~128 KiB *over* `max_payload_bytes` is still broker-admitted, because the advertised limit bounds the whole packet, not the body alone. Size `max_payload_bytes` for the intended body and treat the effective admitted-body ceiling as `max_payload_bytes + ~128 KiB`. With `receive_maximum` this makes the worst-case pending memory `receive_maximum × (max_payload_bytes + slack)` broker-enforced. `0` advertises no packet-size limit (the broker's own max-message policy is the only ceiling). Governs the advertised inbound packet ceiling only; it is not an outbound publish validator. |
| `unmatched_grace` | duration | `30s` | Grace window after **each** connect during which an incoming publish matching no registered receiver filter is buffered (un-acked) awaiting handler registration. After the window a still-unmatched publish is split by whether a wanted subscription still covers its topic. A topic the session still wants whose handler registered late is **retained un-acked** and redelivered once the handler registers (`MQTTRouterCoveredRetained`) — never acked-dropped, so a late-registering live route cannot lose a QoS 1/2 message; only a covered QoS 0 publish the bounded buffer cannot hold is dropped best-effort (`MQTTRouterCoveredDropped`). An orphan topic no configured route covers (a leftover broker-side subscription on a resumed `clean_start=false` session) is acked, dropped, and UNSUBSCRIBEd (deduped, one warn per topic) to converge (`MQTTRouterUnmatchedDropped`, benign cleanup). `0` → `DefaultUnmatchedGrace` (30s). |
| `no_local` | bool | `false` | Opt-in MQTT 5 **No-Local**. When `true`, every **ordinary** subscription is issued with the No-Local flag so the broker does not deliver a message back to the same session that published it — breaking the same-broker MQTT→MQTT self-delivery loop where a session that both subscribes and publishes on overlapping filters would otherwise receive and re-forward its own publishes (unbounded self-amplification). Default `false` preserves the least-surprising MQTT contract (a session receives its own publishes), so existing single-session round-trip topologies are unaffected. A shared subscription (`$share/…`) **never** sets No-Local even when this is `true`: MQTT 5 §3.8.3.1 makes No-Local on a shared subscription a Protocol Error the broker rejects with a DISCONNECT. Cross-bridge delivery is unaffected — No-Local is per-connection and distinct bridges use distinct `client_id`s. See [ADR 0010](../adr/0010-mqtt-loop-prevention-contract.md). |
| `username` | string | -- | Authentication username. Sent in the MQTT CONNECT packet in **cleartext** — see `allow_plaintext_credentials` and use a TLS broker scheme (`ssl://`, `mqtts://`, …). |
| `password` | string | -- | Authentication password (redacted on marshal). Sent in the MQTT CONNECT packet in **cleartext** — protect it with a TLS broker scheme. |
| `allow_plaintext_credentials` | bool | `false` | Opt IN to sending `username`/`password` over a **non-TLS** broker URL (`tcp://`, `mqtt://`, `ws://`, or schemeless). Default `false` **fails closed**: if credentials are configured and any `broker_urls` entry is not a TLS scheme, session build is rejected (the credentials would travel in cleartext). `tls.enable` does **not** satisfy this — autopaho selects TLS from the URL scheme only. Set `true` only for trusted transports (private mesh, TLS-terminating sidecar, or a localhost test broker). |
| `will.topic` | string | -- | Last Will and Testament topic (required when `will` is set; no wildcards) |
| `will.payload` | string | -- | Will message payload |
| `will.qos` | int | `0` | Will QoS (0, 1, or 2) |
| `will.retain` | bool | `false` | Will retain flag |
| `tls.enable` | bool | `false` | Builds the client TLS material (CA / client cert / verification mode) below. **Does not by itself select a TLS transport** — autopaho dials TLS only when the **broker URL scheme** is a TLS scheme (`ssl://`, `mqtts://`, `tls://`, `mqtt+ssl://`, `tcps://`, `wss://`). On a `tcp://` URL the built config is ignored and the connection stays cleartext, so pair `tls.enable: true` with a TLS scheme. |
| `tls.ca_cert_file` | string | -- | CA certificate file path |
| `tls.cert_file` | string | -- | Client certificate file path |
| `tls.key_file` | string | -- | Client private key file path |
| `tls.ca_cert_pem` | string | -- | CA certificate PEM material (in-memory; wins over `ca_cert_file`). Typically supplied by credential rotation. |
| `tls.cert_pem` | string | -- | Client certificate PEM material (requires `key_pem`; wins over `cert_file`) |
| `tls.key_pem` | string | -- | Client private key PEM material (redacted on marshal; requires `cert_pem`) |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

## Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `default_topic` | string | -- | Fallback publish topic used when `OutboundMessage.Address` is empty. The publish topic is never read from `Envelope.Subject`. Validated as an MQTT publish topic at **build time** (wildcards `+`/`#`, a `$`-reserved prefix, and null bytes are rejected), because it bypasses the runtime address validator — a malformed value would otherwise only fail at first publish, as a broker DISCONNECT that tears down the shared session for every route on it. |
| `qos` | int | `1` | MQTT QoS level (0, 1, or 2) |
| `retain` | bool | `false` | MQTT retain flag |
| `timeout` | duration | `30s` | Per-publish timeout, applied as the **stricter** of this value and the caller's remaining deadline. On a bridge route the dispatcher already wraps every send in the route's `policy.send_timeout` (default 30s), so a `timeout` **shorter** than the remaining route deadline tightens the publish while a **longer** one is capped by the route deadline — it never extends the route ceiling. **Note the coercion asymmetry:** unlike an explicit `qos: 0` or `keep_alive: 0` (honoured as-is), a configured `timeout: 0` is coerced **up** to the `30s` default at build. The 60s Send-time safety-net for a zero timeout is therefore reachable only by a direct library consumer that constructs `SenderOptions` and leaves `Timeout` at `0`, bypassing the factory — via config, a `0` becomes `30s`. See [Resilience Behavior](#resilience-behavior) for the interaction with `policy.send_timeout`. |
| `throttle_retry_after` | duration | `500ms` | Retry-after hint attached to a publish failure **only** when the broker returns PUBACK/PUBREC reason `0x97` (Quota exceeded) -- the one reason code that signals throttling. Other non-zero reason codes classify as generic errors with no back-off hint. |

## Credential URI (`options.credentials_uri`)

`credentials_uri` is a top-level key under `options:`, a sibling of `session:`
and `sender:` (not a session key). It names a credential-store URI (`file://…`,
`pms://…`) that the runtime credential resolver reads at build time. The resolver
merges the resolved `username`, `password`, and TLS material
(`tls_cert`/`tls_key`/`tls_ca`) into the session options and removes the
`credentials_uri` key before the MQTT transport sees the config, so the secrets
never appear in the YAML. See
[HTTP ingress with credentials](../scenarios/15-http-ingress-with-credentials.md)
for the resolver walkthrough.

Token-style auth is supported: a password (or bearer token) with no username
sets the CONNECT password flag independently of the username flag, so a
token-in-password credential is no longer dropped for want of a username.

## Settlement Semantics

> **QoS 2 is NOT exactly-once across a bridge restart.** autopaho keeps the
> outbound packet queue **in memory**, so an in-flight QoS 1/2 publish (sent,
> PUBACK/PUBCOMP not yet received) is lost at the MQTT-protocol level on a crash
> or restart — `client_id` / `clean_start=false` resume *broker-side* state, not
> the *client-side* outbound queue. This is not bridge-level loss: the wired
> delivery modes (`direct_hold`, `shared_outbox`) recover the message on the
> source side and the dedup layer collapses the redelivered duplicate (detailed
> below). Operators evaluating an end-to-end exactly-once claim must account for
> this — see also [ADR 0009](../adr/0009-durable-outbound-mqtt-session-state.md).

MQTT deliveries are acknowledged **after** the bridge settles them, not on
receipt. The adapter connects with manual acknowledgement and holds the PUBACK
(QoS 1) / PUBCOMP (QoS 2) until the runtime acks the delivery — after the
downstream send or outbox persist succeeds. Acks are released in receive order,
so an in-flight message survives a crash and is redelivered by the broker when
a Persistent/Exclusive session resumes.

**Ephemeral sessions have a loss window.** An Ephemeral session keeps no offline
retention: during any disconnect the broker queues nothing for it, so messages
it would have delivered are lost with no redelivery on reconnect, and a runtime
reconfig swap leaves an unavoidable delivery gap. Outbound in-flight QoS 1/2
state lives only in memory and is lost on a bridge restart or crash. Persistent
and Exclusive sessions (`clean_start=false` with a non-zero
`session_expiry_interval`) close the offline and reconfig gaps — the broker
queues inbound deliveries while the client is away and redelivers them on
resume — but they do **not** make that outbound in-flight QoS 1/2 state
durable: a bridge restart or crash still loses it, because it never leaves the
in-memory packet store.

**MQTT QoS 1/2 alone is not durable egress — but neither wired delivery mode
loses a message.** autopaho keeps the outbound packet queue **in memory**, so a
publish that is in flight (sent, PUBACK/PUBCOMP not yet received) when the
process dies is lost at the *protocol* level, and MQTT QoS 2 is **not**
exactly-once across a restart. `client_id` / `clean_start=false` do not help —
they resume *broker-side* state, not the *client-side* outbound queue. What
saves the message is where the route acknowledges the **source**:

- **`direct_hold`** (the default) holds the source delivery un-acked until the
  broker returns PUBACK (QoS 1) / PUBCOMP (QoS 2). A crash before that ack leaves
  the source message un-acked, so the source redelivers it — the lost in-flight
  publish is re-sent on recovery. No bridge-level loss.
- **`shared_outbox`** invokes the sender from a version-fenced persisted outbox
  record and marks it complete only **after** the send returns. A crash before
  completion replays the record on restart, so the publish is re-sent
  (idempotency keys collapse the duplicate). No bridge-level loss.

So the in-memory packet store means MQTT-protocol QoS 2 exactly-once is not
preserved across a restart — a redelivered duplicate is collapsed by the
bridge's dedup — but it does **not** translate into lost messages on either
delivery mode. The only way an in-flight loss could become bridge-level loss is
a delivery mode that acks the source *before* the transport confirms the
publish; no such mode exists today, so the bridge emits a route-aware startup
advisory (`bridge.egressDurabilityAdvisory`) that stays silent for both current
modes and exists only to flag such a future mode. Do **not** treat MQTT QoS 1/2
as the sole loss-prevention for outbound traffic — pair loss-sensitive egress
with `shared_outbox` (or the redelivery-backed `direct_hold` above). (A
file-backed Paho session store is a deferred, ADR-level alternative and is not
wired today.)

## Resilience Behavior

- **Publish timeout — route policy vs. sender timeout.** The sender applies the
  **stricter** of `options.sender.timeout` and the caller's remaining context
  deadline. On a bridge route the dispatcher always wraps each send in the route's
  `policy.send_timeout` (default 30s), so that deadline is the ceiling: a
  `sender.timeout` **shorter** than the remaining route deadline tightens the
  publish (useful for a route that must fail fast to a slow broker), while a
  **longer** `sender.timeout` is capped by the route deadline and does not extend
  it. The 60-second safety-net fires **only** when there is no caller deadline at
  all — i.e. a direct library consumer that calls `Send` without a route
  dispatcher and leaves `timeout` at `0`. In a bridge deployment `sender.timeout`
  therefore only ever *tightens* a send; it cannot loosen the route ceiling.
- **Case-insensitive error classification.** MQTT error messages from brokers
  are matched case-insensitively. `"Connection Refused"`, `"CONNECTION REFUSED"`,
  and `"connection refused"` are all correctly classified as `ErrConnectionLost`,
  enabling proper retry behavior regardless of broker formatting. This matching
  is a **substring table over SDK error strings**, correct against the pinned
  `paho.golang v0.23.0`. **Maintenance/upgrade checklist:** on any paho.golang
  bump, re-verify the `MapError` string table (`errors.go`) — a reworded SDK
  error can silently fall through to the `ErrUnavailable` default and change
  retry behavior.
- **Properties deep-copy for shared sessions.** When multiple receivers share an
  MQTT session, each handler goroutine receives an independent deep-copy of the
  MQTT Properties (User properties, CorrelationData, ContentType, etc.),
  preventing data races under concurrent dispatch.
- **Password rotation rebuilds the session.** Applying a rotated password calls
  `Session.Reload`, which tears down and rebuilds the connection manager so a
  fresh CONNECT carries the new credentials. It does **not** call
  `ConnectionManager.Disconnect`: in paho.golang v0.23.0 that cancels the CM
  root context and is terminal -- the client never reconnects and `Health()`
  would still report the session up. TLS material rotates through the same
  `Reload` path. See [Credential Rotation](../credentials-rotation.md).
- **Rotation during an outage recovers on its own.** A credential or TLS rotation
  `Reload` that fails because the broker is unreachable during the outage no
  longer leaves the session permanently dead. The session signals terminal death
  and the runtime supervisor re-Starts it (with jittered backoff), so it
  reconnects by itself once the broker returns.
- **Granted-QoS downgrade is surfaced.** When the broker grants a subscription a
  lower QoS than requested (a SUBACK reason below the requested level, e.g.
  requested QoS 2, granted QoS 0), the route still assumes the requested
  guarantee, so the downgrade silently removes offline/redelivery coverage and
  opens a disconnect-gap loss window. The reconcile loop stores the requested QoS
  as its delta baseline (a stable downgraded sub is not re-subscribed every cycle)
  and counts `MQTTQoSDowngraded` with a loud warning once per subscription
  transition — initial subscribe, reconnect, or a plan that changes the requested
  QoS. Any non-zero value warrants checking the broker's QoS-cap policy.
- **Retained replay is suppressed on reconnect.** Persistent and Exclusive
  re-subscribes carry MQTT 5 **Retain Handling = 1** ("send retained only if the
  subscription did not already exist"), so a `clean_start=false` session resuming
  broker-side state is not flooded with a retained-message replay for every
  filter on every reconnect — the retained set already delivered on the first
  subscribe would otherwise re-enter the pending buffer as a thundering backlog.
  Ephemeral sessions use Retain Handling = 0 (always send retained): each connect
  is a fresh subscription with no prior broker-side state to dedupe against, so
  the initial retained snapshot is the intended first-delivery.

## Backpressure and dispatch

The publish callback paho invokes must return quickly or the client stops
servicing PINGRESP/PUBACK and the connection dies of keepalive starvation. The
adapter therefore hands each inbound publish to a serialized dispatch queue and
returns:

- The **dispatch queue** holds up to `defaultDispatchSize` (1024) items. When it
  is full under a flood, a **QoS 0** publish is dropped (`MQTTRouterDropped`,
  logged) because QoS 0 carries no delivery contract; a **QoS 1/2** publish
  blocks until a slot drains (bounded by the broker's Receive-Maximum window), so
  at-least-once is preserved as broker backpressure.
- The **pre-registration pending buffer** absorbs the CONNACK backlog that
  arrives before receivers register (see [Session Modes](#session-modes)). It has
  two independent bounds applied asymmetrically by QoS: an entry-count cap sized
  to `receive_maximum` (default **1024**) and a **64 MiB** payload ceiling
  (`defaultPendingBytesLimit`). The byte ceiling governs **QoS 0 only**. A QoS 0
  publish over either cap is dropped (`MQTTRouterDropped`) — it carries no
  delivery contract. A **QoS 1/2** publish is never dropped for the byte ceiling:
  it evicts the oldest QoS 0 entry to reclaim memory and buffers regardless,
  bounded by the count cap. QoS 1/2 memory needs no byte cap because the broker's
  Receive-Maximum flow control never delivers more than `receive_maximum` un-acked
  QoS 1/2 at once, so at most `receive_maximum × max_payload_bytes` of QoS 1/2 is
  ever pending. The single path that drops a QoS 1/2 publish is the count cap hit
  with no QoS 0 left to evict — reachable only when a broker exceeds the Receive
  Maximum it was granted (a protocol violation). That publish is acked-and-dropped
  (dropping-with-ack keeps paho's in-order ack stream draining) and counted on
  `MQTTRouterOverflowDropped`, so any non-zero value points at a broker bug, not
  operator mis-sizing. Publishes held in the buffer count on `MQTTRouterBuffered`.

> **Byte accounting is partial.** The dispatch queue is bounded by **count**
> (1024 items), and paho's in-flight `publishPackets` window is bounded by
> `receive_maximum` -- neither is byte-bounded on its own. A burst of
> large-payload QoS 1/2 publishes can pin roughly `(1024 + receive_maximum) ×
> payload` bytes in memory before the blocking backpressure engages -- 1024 is the
> dispatch-queue count ceiling, the second term is paho's in-flight window.
> Because `receive_maximum` now defaults to **1024** instead of 65535, the default
> worst case falls from `(1024 + 65535) × payload` to `(1024 + 1024) × payload` --
> roughly 32× smaller. Bound the `payload` term too: set `max_payload_bytes` and
> the adapter advertises an MQTT v5 Maximum Packet Size in CONNECT, derived from
> that value plus a ~128 KiB protocol-overhead allowance and clamped to the MQTT
> v5 four-byte ceiling (256 MiB − 1). The broker is then forbidden from delivering
> a larger packet, so the `receive_maximum × max_payload_bytes` bound is
> broker-enforced. Left unset (`0`) it advertises no packet-size limit -- the
> broker's own max-message policy is the only ceiling.

## Shared subscriptions (`$share`)

MQTT declares the `shared_consumer` capability. A subscription filter of the
form `$share/<group>/<filter>` is a shared subscription: the broker
load-balances the topic's deliveries across every client in `<group>`, so
several bridge instances (or several receivers) consume one logical subscription
as a scale-out group instead of each receiving a full copy.

Declare the shared filter in the receiver's `topics[]` exactly as the broker
expects it (`$share/<group>/<filter>`). The adapter strips the `$share/<group>/`
prefix before matching, so routing keys off the concrete topic the broker
delivers on, not the `$share` wrapper. Ordinary (non-shared) subscriptions are
unaffected.

### Each scale-out instance needs a UNIQUE `client_id`

Shared-subscription scale-out and `client_id` interact in a way that is easy to
misconfigure into a self-DOS. The broker load-balances a `$share` group across
**distinct sessions**, and a session is keyed by `client_id`. So:

- **Scale-out (multiple active consumers): give every replica a UNIQUE
  `client_id`.** Two live instances that reuse one `client_id` are, to the
  broker, the *same* session — the second connect triggers a **session
  takeover** (MQTT `0x8E`) that kicks the first off, which reconnects and kicks
  the second off, and so on. The result is a reconnect storm that consumes
  nothing (self-DOS), not load-balancing. Use `session_mode: ephemeral` (unique
  `client_id` + `clean_start=true`) and give each replica a distinct `client_id`
  — set `client_id_suffix: hostname` (or `nonce`) so one shared config file still
  yields a unique id per pod (recipe below).
- **A shared/stable `client_id` is only safe behind an exclusive lease.** In
  `session_mode: exclusive`, one holder connects at a time (the lease
  guarantees it), so a stable `client_id` is correct and a takeover is a
  legitimate lease **failover**. But with a single active holder a `$share`
  group has exactly one member, so it **serialises** rather than scales the
  subscription.

The adapter cannot see the other replicas' `client_id`s from one process, so it
**detects the symptom**: when `$share` subscriptions are configured on a
non-Ephemeral session it warns once about the unique-`client_id` requirement,
and a session takeover while `$share` is active (outside Exclusive mode) is
logged at **Error** on the first occurrence — that combination is the
smoking-gun of a reused `client_id`. `MQTTSessionTakeover` counts every
takeover; a persistent non-zero rate on a `$share` deployment means the
`client_id`s are colliding.

#### Recipe: unique `client_id` per replica from one config file

A Kubernetes Deployment or ECS service scales one config (ConfigMap / task
definition) to `replicas: N`, so every pod reads the **same** `client_id` — the
self-DOS above. `client_id_suffix` resolves it at build time without per-pod
templating:

```yaml
sessions:
  - id: telemetry-in
    transport: mqtt
    session_mode: ephemeral        # scale-out, NOT exclusive
    options:
      session:
        broker_url: tls://mqtt.prod.example.com:8883
        client_id: telemetry-consumer   # shared base in the ConfigMap
        client_id_suffix: hostname       # -> telemetry-consumer-<pod name>
        clean_start: true
```

- `hostname` appends the container/pod hostname (`telemetry-consumer-web-7d9f-abc12`).
  On K8s the pod name is already unique and stable for the pod's life, and it
  shows up verbatim in broker logs — prefer it when the hostname is distinct
  (the default for Deployments and StatefulSets).
- `nonce` appends 8 hex characters from `crypto/rand`, unique per **process**.
  Use it when replicas do not have distinct hostnames (some flat container
  networks) — but note a pod restart yields a *new* id, so the broker sees a new
  ephemeral session rather than a resumed one (fine for `clean_start: true`).

> **Do not set `client_id_suffix` on an exclusive session.** Exclusive failover
> needs a *stable shared* `client_id` so the standby can resume the dead owner's
> broker session; a per-instance suffix would strand queued QoS 1/2 messages on
> every failover. The build **rejects** `client_id_suffix` when `session_mode:
> exclusive`. See [scenario 08](../scenarios/08-clustered-exclusive-sessions.md).

## Ingress headers (`mqtt.*`)

At ingress the adapter stamps the delivered MQTT metadata onto the envelope
headers under the reserved `mqtt.*` namespace:

| Header | Value | Meaning |
|--------|-------|---------|
| `mqtt.topic` | string | The concrete topic the broker delivered on (distinct from the logical subject carried in the `gobridge.subject` user property). |
| `mqtt.retained` | bool | Whether the broker delivered the publish with the retained flag set. |
| `mqtt.qos` | int | The delivered QoS level (0, 1, or 2). |

These keys are bridge-owned. An inbound MQTT user property whose name collides
with `mqtt.topic`, `mqtt.retained`, or `mqtt.qos` is dropped during conversion,
so a remote publisher cannot spoof the delivered topic, retained state, or QoS.
Read them downstream to branch on retained snapshots or on the QoS a message
arrived at.

### User-property ingestion and the header filter

Beyond the reserved `mqtt.*` keys, every MQTT v5 **user property** on an inbound
publish is copied onto the envelope headers under its own name, so a route can
filter or branch on peer-supplied metadata. Two admission rules apply per
property, and a value that fails either is **dropped** (the key never appears on
the envelope):

- **Length.** A key or value longer than 256 **bytes** is dropped (a bound on
  per-message header memory; note this is UTF-8 byte length, so a multi-byte
  value reaches the cap in fewer visible characters).
- **Content.** Both the key and the value must be **valid UTF-8 with no control
  characters**. MQTT v5 user properties are UTF-8 string pairs by spec, so
  ordinary non-ASCII text is preserved — `location: Malmö` arrives intact. Only
  genuinely unsafe values (invalid UTF-8, or embedded control characters such as
  `NUL`, newline, or other `unicode.IsControl` runes) are rejected, matching the
  reserved-header safety model.

Every dropped user property is counted on **`MQTTIngressHeaderDropped`**, the
ingress counterpart to egress's `MQTTNonStringHeaderDropped`. A non-zero rate
means a peer is sending headers the bridge cannot admit (over-length or unsafe);
watch it if a route that filters on a peer header starts misrouting — a silent
drop here is now observable rather than invisible. (Reserved-key collisions
described above are a separate, deliberate anti-spoof drop and are not counted on
this metric.)

### Envelope identity and no-ID redelivery

Inbound identity uses this precedence:

1. a valid `mqtt.message-id` user property, which GoBridge peers stamp from
   `Envelope.ID`;
2. valid MQTT correlation data;
3. an RFC 4122 UUIDv4 generated once for the received publish and stamped on the
   router-owned Paho publish before buffering or fan-out.

Every handler reached by one publish therefore sees the same generated
`Envelope.ID`. Two separate publishes receive separate IDs even when their topic
and payload bytes are identical. Packet ID, topic, payload, QoS, and DUP are
never fallback identity inputs: packet IDs are reusable within an MQTT session
and none of those fields proves application-event identity.

A broker redelivery without `mqtt.message-id` or correlation data may receive a
new ID and therefore duplicate downstream. MQTT cannot prove that a no-ID
publish is the same application event across reconnect and packet-ID reuse.
GoBridge deliberately accepts that at-least-once duplicate because delivering a
possible duplicate is safer than silently collapsing two legitimate equal-valued
publishes in `shared_outbox`. Producers that require stable deduplication across
redelivery must provide a stable `mqtt.message-id` (preferred) or correlation
identity and reuse it for every delivery attempt.

## Receiver Options

MQTT receivers have no transport-specific options. Subscriptions are declared
in the `topics[]` array on the `ReceiverDef`, not in the `options` map.

> **Receiver IDs must be unique (library-consumer note).** A receiver registers
> its topic filters on the shared session keyed by its **ID**. Constructing two
> receivers with the *same* ID on one session makes the second silently overwrite
> the first's registration (and an unregister for that ID removes whichever is
> current), so one of them stops receiving with no error. In a bridge deployment
> the runtime guarantees unique route/receiver IDs, so this cannot happen from
> YAML — it is a hazard only for code that drives the adapter directly. Give every
> receiver a distinct ID.

---

