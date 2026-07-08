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

On a resumed (`clean_start=false`) session the broker replays its queued
backlog on CONNACK before the route runners have registered their topic
filters, so briefly some publishes match no handler. Those are buffered for
`unmatched_grace` (default 30s, restarted on every reconnect). After the
window a still-unmatched publish splits two ways. If a route the session
still wants covers the topic — its receiver handler only registered late —
the adapter acks-and-drops it and counts `MQTTRouterCoveredDropped`: **real
message loss** on a live route, so any non-zero value is alarming. Otherwise
it is an **orphan subscription** — typically a route removed from config
whose broker-side subscription survived the resume — and the adapter
acks-and-drops it, UNSUBSCRIBEs its exact topic, and counts
`MQTTRouterUnmatchedDropped` (benign cleanup), so the orphan no longer stalls
in-order acking for the rest of the shared session. See [`paho/doc.go`](../../adapters/mqtt/transport/paho/doc.go)
for the full mechanism.

## YAML Example

```yaml
sessions:
  - id: mqtt-session-1
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "tcp://broker.example.com:1883"
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
| `client_id` | string | -- | MQTT client identifier. **Required** on the effective (merged) session config at build time, together with at least one broker URL (`factory.go:59-66`, `NewSession`); an empty value is accepted at parse time. |
| `keep_alive` | int | `30` | Keep-alive interval in seconds. Explicit `0` disables the pinger. |
| `connect_timeout` | duration | `30s` | Bounds the **initial** Start connection await |
| `reconnect_timeout` | duration | `30s` | Bounds each individual (re)connect attempt (TCP dial + TLS + CONNECT/CONNACK). Maps to autopaho `ConnectTimeout`; `0` → autopaho default (10s). |
| `reconnect_delay` | duration | `10s` (`DefaultReconnectDelay`) | **Base** delay of a jittered exponential reconnect backoff. Starts at `reconnect_delay`, grows 2× (`reconnectBackoffFactor`) per failed attempt, caps at `reconnect_max_delay`, then equal-jitters to `[d/2, d)` to desynchronise a reconnecting fleet (anti thundering-herd). `0` → `10s`. |
| `reconnect_max_delay` | duration | `2m` (`DefaultReconnectMaxDelay`) | Caps the jittered-exponential reconnect envelope. Must be ≥ `reconnect_delay`; a smaller value is clamped up to the base at Start. `0` → `2m`. |
| `clean_start` | bool | `false` | MQTT 5 clean-start flag; consulted only for Persistent/Exclusive sessions |
| `session_expiry_interval` | int | `0` | MQTT 5 session expiry in seconds. For Persistent/Exclusive sessions a `0` is replaced at Start with `86400` (24h) — a literal `0` would give zero offline retention. Ephemeral always uses `0`. |
| `receive_maximum` | int | `0` → **1024** (`DefaultReceiveMaximum`) | MQTT 5 Receive Maximum: max in-flight QoS 1/2 messages the broker may send before PUBACKs. `0` is not a legal MQTT v5 value, so an unset/`0` is coerced to **1024** to bound worst-case buffered memory — set it explicitly to `65535` for the old protocol ceiling. Bounds only the QoS 1/2 un-acked window — it does **not** throttle QoS 0. Also sizes the startup pending buffer used during `unmatched_grace`. |
| `unmatched_grace` | duration | `30s` | Grace window after **each** connect during which an incoming publish matching no registered receiver filter is buffered (un-acked) awaiting handler registration. After the window a still-unmatched publish is acked and dropped, split by whether a wanted subscription still covers its topic. A topic the session still wants whose handler registered late is **real message loss** on a live route (`MQTTRouterCoveredDropped` — any non-zero value is alarming). An orphan topic no configured route covers (a leftover broker-side subscription on a resumed `clean_start=false` session) is benign cleanup (`MQTTRouterUnmatchedDropped`), and its exact topic is UNSUBSCRIBEd (deduped, one warn per topic) to converge. `0` → `DefaultUnmatchedGrace` (30s). |
| `username` | string | -- | Authentication username |
| `password` | string | -- | Authentication password (redacted on marshal) |
| `will.topic` | string | -- | Last Will and Testament topic (required when `will` is set; no wildcards) |
| `will.payload` | string | -- | Will message payload |
| `will.qos` | int | `0` | Will QoS (0, 1, or 2) |
| `will.retain` | bool | `false` | Will retain flag |
| `tls.enable` | bool | `false` | Enable TLS |
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
| `default_topic` | string | -- | Fallback publish topic used when `OutboundMessage.Address` is empty. The publish topic is never read from `Envelope.Subject`. |
| `qos` | int | `1` | MQTT QoS level (0, 1, or 2) |
| `retain` | bool | `false` | MQTT retain flag |
| `timeout` | duration | `30s` | Per-publish timeout |
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

## Resilience Behavior

- **Publish timeout fallback.** When `timeout` is set to `0` (or omitted and no
  context deadline is present), the sender applies a 60-second safety-net
  timeout to prevent indefinite hangs on stalled broker connections.
- **Case-insensitive error classification.** MQTT error messages from brokers
  are matched case-insensitively. `"Connection Refused"`, `"CONNECTION REFUSED"`,
  and `"connection refused"` are all correctly classified as `ErrConnectionLost`,
  enabling proper retry behavior regardless of broker formatting.
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
  arrives before receivers register (see [Session Modes](#session-modes)). It is
  bounded by both an entry count sized to `receive_maximum` (default **1024**)
  and a **64 MiB** payload ceiling (`defaultPendingBytesLimit`). Publishes held
  here count on `MQTTRouterBuffered`;
  a QoS 0 publish that would overflow the buffer is dropped (`MQTTRouterDropped`),
  while a QoS 1/2 publish evicts the oldest QoS 0 entry to make room.

> **Byte accounting is partial.** The dispatch queue is bounded by **count**
> (1024 items), and paho's in-flight `publishPackets` window is bounded by
> `receive_maximum` -- neither is byte-bounded. Only the pending buffer enforces
> a byte ceiling. A burst of large-payload QoS 1/2 publishes can therefore pin
> roughly `(1024 + receive_maximum) × payload` bytes in memory before the
> blocking backpressure engages -- 1024 is the dispatch-queue count ceiling, the
> second term is paho's in-flight window. Because `receive_maximum` now defaults
> to **1024** instead of 65535, the default worst case falls from
> `(1024 + 65535) × payload` to `(1024 + 1024) × payload` -- roughly 32× smaller.
> Bound it further operationally: cap `receive_maximum` and the upstream payload
> size for large-message workloads.

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

## Receiver Options

MQTT receivers have no transport-specific options. Subscriptions are declared
in the `topics[]` array on the `ReceiverDef`, not in the `options` map.

---

