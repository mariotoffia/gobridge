# MQTT options reference

Field-by-field reference for the MQTT transport: session, sender and
receiver options, credential URIs, and mutual TLS.

See [MQTT](mqtt.md) for sessions and a worked example, and
[MQTT behaviour](mqtt-behavior.md) for settlement, resilience and
backpressure.

---

## Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- | Single broker URL (e.g. `tcp://host:1883`). Folded into `broker_urls` when the list form is absent. |
| `broker_urls` | []string | -- | Broker URLs for failover. Ephemeral sessions may use multiple independent URLs. Persistent/exclusive sessions reject more than one distinct canonical URL because one managed-filter history cannot safely span independent broker-session domains. **Canonical** means the endpoint actually dialled: scheme aliases collapse (`tcp`/`mqtt`, and `ssl`/`tls`/`mqtts`/`mqtt+ssl`/`tcps`), an omitted port becomes the family default (1883 / 8883 / 80 / 443), host case, userinfo and fragment are ignored, and path/query count only for `ws`/`wss`. Two durable sessions spelled differently but reaching one endpoint are rejected as duplicate identities at startup instead of disconnecting each other on their shared `client_id`. A scheme outside that list is rejected. |
| `client_id` | string | -- | MQTT client identifier. **Required** on the effective (merged) session config at build time, together with at least one broker URL (`Config.ValidateEffectiveSession` in `config_plugin.go`, enforced by `Factory.NewSession`); an empty value is accepted at parse time. For scale-out uniqueness from one shared config file, see `client_id_suffix`. |
| `client_id_suffix` | string | -- | Opt-in per-instance uniquifier appended to `client_id`, required for clustered non-Exclusive `$share` consumers. `hostname` appends the process-cached hostname; for **Persistent** sessions it is safe **only where hostnames are stable across restarts** (StatefulSet/VM — NOT Deployments/ECS, where every rollout orphans the previous broker session and its queued QoS 1/2; the factory **rejects** this combination unless `assert_stable_client_identity: true` — see [Deployment identity](#deployment-identity)). `nonce` appends a process-cached random token and is allowed only for Ephemeral sessions. Unset leaves `client_id` verbatim. Exclusive rejects every suffix because failover resumes one stable shared client ID. |
| `keep_alive` | int | `30` | Keep-alive interval in seconds. Explicit `0` disables the MQTT pinger — half-open-connection detection then rests on TCP keep-alive alone (much slower, and OS-dependent), so a dead-but-open socket can go unnoticed for minutes. The registry/blueprint path defaults to `30`; a direct library consumer that sets `0` should understand this trade-off. |
| `connect_timeout` | duration | `30s` | Bounds the **initial** Start connection await |
| `reconnect_timeout` | duration | `30s` | Bounds each individual (re)connect attempt (TCP dial + TLS + CONNECT/CONNACK). Maps to autopaho `ConnectTimeout`; `0` → autopaho default (10s). |
| `reconcile_timeout` | duration | `30s` (`DefaultReconcileTimeout`) | Bounds **each** broker SUBSCRIBE / UNSUBSCRIBE issued while reconciling the session plan. The reconcile runs on a possibly deadline-less runtime context, so without this an unresponsive broker (SUBACK/UNSUBACK never arrives on a half-open connection) would hang the reconcile — and any startup / hot-reload step awaiting it — indefinitely. This is a **liveness safety bound**: a non-positive value is coerced **up** to `30s` and cannot be disabled. |
| `reconnect_delay` | duration | `10s` (`DefaultReconnectDelay`) | **Base** delay of a jittered exponential reconnect backoff. Starts at `reconnect_delay`, grows 2× (`reconnectBackoffFactor`) per failed attempt, caps at `reconnect_max_delay`, then equal-jitters to `[d/2, d)` to desynchronise a reconnecting fleet (anti thundering-herd). `0` → `10s`. |
| `reconnect_max_delay` | duration | `2m` (`DefaultReconnectMaxDelay`) | Caps the jittered-exponential reconnect envelope. Must be ≥ `reconnect_delay`; a smaller value is clamped up to the base at Start. `0` → `2m`. |
| `clean_start` | bool | `false` | MQTT 5 clean-start flag; consulted only for Persistent/Exclusive sessions. **`clean_start: true` on a Persistent session wipes the broker-side session (subscriptions AND queued offline QoS 1/2) on every process restart** — the backlog the mode exists to retain is discarded each time. Honoured as configured, with a construction-time warning; on Exclusive it is overridden to `false` (takeover loop). |
| `session_expiry_interval` | int | `0` | MQTT 5 session expiry in seconds. For Persistent/Exclusive sessions a `0` is replaced at session creation (`NewSession`) with `86400` (24h) — a literal `0` would give zero offline retention. Ephemeral always uses `0`. |
| `receive_maximum` | int | `0` → **192** (`DefaultReceiveMaximum`) | MQTT 5 Receive Maximum: max in-flight QoS 1/2 messages the broker may send before PUBACKs. `0` is normalized because it is illegal on the wire. The same effective value sizes one reservation shared by the serialized dispatch queue and startup/migration pending entries; those stores cannot each retain a full independent window. An explicitly configured non-zero value receives full window validation during parse and is rejected when unsafe. An omitted value stays unmaterialized during parse so a deployment profile may derive a lower safe value; generic bridge preflight later applies 192 and performs the same full validation. |
| `max_payload_bytes` | int | `0` → **262144** (`DefaultMaxPayloadBytes`) | Maximum inbound application body, in bytes. CONNECT advertises a separate wire Maximum Packet Size of this body limit plus a 128 KiB MQTT v5 metadata allowance. After TLS/WebSocket decoding but before Paho packet decoding, an adapter-owned connection guard frames one bounded wire packet, validates Remaining Length before allocation, and rejects an oversized body, a property block over 128 KiB, more than 128 structurally parsed User Properties, or topic-plus-properties metadata over 128 KiB. The decoded callback repeats the retained-representation checks as defense in depth. A violation terminally recycles the session before SDK acknowledgement tracking or adapter queues can retain the packet. Values too large to retain the metadata allowance below the MQTT 256 MiB − 1 packet ceiling are rejected, never clamped. This does not limit outbound publishes. |
| `ingress_memory_budget_bytes` | int | `0` → **268435456** (`DefaultIngressMemoryBudgetBytes`) | Per-session conservative MQTT ingress budget (256 MiB). The bridge validates the full packet/window equation below using the route's effective `max_in_flight` before opening stores or transports. Validation includes ReceiverDef-backed sessions with no consuming route and referenced Persistent/Exclusive sessions that can resume stale backlog. Exact boundary is accepted; one byte over budget and every arithmetic overflow are rejected as invalid config. |
| `unmatched_grace` | duration | `30s` | Grace window after **each** connect during which an incoming publish matching no registered receiver filter is buffered (un-acked) awaiting handler registration. It is also the post-recycle no-replay verification window for managed-filter removal; a pinned matching replay or a shorter reconciliation deadline fails migration closed and preserves history. After the window a still-unmatched publish is split by whether a wanted subscription still covers its topic. A topic the session still wants whose handler registered late is **retained un-acked** and redelivered once the handler registers (`MQTTRouterCoveredRetained`) — never acked-dropped, so a late-registering live route cannot lose a QoS 1/2 message; only a covered QoS 0 publish the bounded buffer cannot hold is dropped best-effort (`MQTTRouterCoveredDropped`). An orphan topic no configured route covers (a leftover broker-side subscription on a resumed `clean_start=false` session) is acked, dropped, and UNSUBSCRIBEd (deduped, one warn per topic) to converge (`MQTTRouterUnmatchedDropped`, benign cleanup). `0` → `DefaultUnmatchedGrace` (30s). |
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

### Duration validation

Every duration above accepts `0`, which selects the documented default. A
**negative** value is rejected at configuration validation, so it can never
reach a session. Nothing downstream treats a negative duration as an error --
it becomes an already-expired context -- so a build that accepted one would
start successfully and then fail every attempt it made, for a reason that is
invisible in the configuration.

### Packet acknowledgement budget

The MQTT client applies its own deadline to each packet acknowledgement it
waits for (CONNACK, SUBACK, UNSUBACK, PUBACK/PUBCOMP), *inside* the deadline
the bridge already set. Its built-in default is 10 seconds -- shorter than
every budget on this page -- so leaving it alone would silently override them:
a SUBACK the bridge was willing to wait 30 seconds for would be abandoned at
10, failing a reconcile while the broker was answering normally.

There is no key for it. The session derives the budget as the longest of
`connect_timeout`, `reconnect_timeout`, `reconcile_timeout`, the `timeout` of
every sender bound to the session, and the 30-second sender default -- so the
adapter-owned bound is always the one that governs. It is not a liveness bound
of its own: each packet operation already runs under its own deadline, and that
is what bounds an unresponsive broker.

One consequence is worth knowing when tuning shutdown: a SUBSCRIBE or
UNSUBSCRIBE still in flight when the session is closed now waits out
`reconcile_timeout` rather than being cut short at the client's old 10-second
default. Cancelling the context passed to `Reconcile` still ends it
immediately, which is what the runtime does on shutdown, so this is visible
only to a library consumer that closes a session while holding a longer-lived
context open.

### Ingress byte model

Every MQTT session that can own inbound state is validated independently:

```text
packet   = ceil(decodedPacketSize(maxPayloadBytes) * 1.25)
crossing = ceil((wirePacketSize(maxPayloadBytes) + transientDecodedPacketSize(maxPayloadBytes)) * 1.25)
window   = receiveMaximum + dispatchCapacity + routeMaxInFlight
bound    = packet * window + crossing
```

`dispatchCapacity` is the effective `receiveMaximum`, not a fixed queue size.
One reservation is shared by dispatch and startup/migration pending entries, so
their combined distinct queued packets never exceed that capacity.
`wirePacketSize` is the separately advertised MQTT Maximum Packet Size: payload
plus a 128 KiB allowance covering the fixed-header byte, worst-case four-byte
Remaining Length encoding, maximal 65,535-byte topic plus its two-byte length,
QoS packet identifier, worst-case properties-length encoding, and bounded
property bytes. `decodedPacketSize` adds both Paho User Property struct
representations and a 32 KiB fixed allowance for SDK structures, accepted
Envelope header-map buckets, outbox/queue state, and allocator page/size-class
rounding. The 25% factor covers remaining Go object and slice bookkeeping.

The two decoded terms use **different property budgets**, and the difference is
load-bearing. `decodedPacketSize` — the per-slot **retained** cost — budgets 128
User Properties, because a packet exceeding that cap is acked-and-dropped by the
publish callback before anything retains it. `transientDecodedPacketSize` — used
only by `crossing` — budgets the **wire** worst case of 26,214 properties. The
CONNECT advertises only a whole-packet Maximum Packet Size, so a compliant
broker may forward a packet whose 128 KiB metadata section is filled entirely
with five-byte (empty key, empty value) User Properties. Paho materialises every
one of them, twice, before the callback can refuse the packet. Budgeting that
worst case per retained slot would multiply the bound by three orders of
magnitude; budgeting it once, for the single decode in flight, is both correct
and affordable.

The single `crossing` term is the formula's `+1` ownership slot. It covers one
complete raw packet buffered by the predecode connection guard plus Paho's
worst-case decoded representation while that wire packet is consumed. For a
rejected packet only the raw half exists, so the same term is conservative.
The guard checks the advertised Maximum Packet Size from Remaining Length before
allocating and never buffers a second packet. Envelope, no-processor route, and
outbox fan-out clones share immutable payload backing; a processor that calls
`SetPayload` owns a new copy. Checked division and overflow guards run before
every addition or multiplication.

The typed parser intentionally keeps zero/unset ingress fields unmaterialized
through config clone/parse. When `receive_maximum` is omitted, parse validates
only receive-independent packet and minimum-budget prerequisites. The AWS
profile then assigns a per-session budget and derives a safe Receive Maximum.
An explicit non-zero Receive Maximum receives full validation immediately and
is never clamped. Independently of deployment profile, `bridge.Builder`
performs full `ValidateIngressMemory(routeMaxInFlight)` preflight before opening
stores or transports; generic composition therefore applies default 192 and
rejects a window that the default 256 MiB budget cannot hold.

The defaults (256 KiB payload, Receive Maximum 192, route `max_in_flight` 100)
produce a 265,797,600-byte bound, below the 256 MiB default budget. Raising
payload size, Receive Maximum, or route concurrency may require a larger budget.
Do not tune only the message count. A budget smaller than one `crossing` slot
(about 3 MiB at the default payload size) is rejected outright: the session
could not decode a single legal packet.

The AWS file-based profile reserves 25% of the effective Fargate task memory,
divides it across unique included MQTT sessions, and derives the largest safe
default Receive Maximum with this same formula. Every started memory-aware
session referenced by a `ReceiverDef` is included, even when no route consumes
the receiver, because its effective session plan can subscribe and admit
traffic. Every referenced Persistent or Exclusive session is also included:
resumed durable broker state may deliver stale backlog before
managed-subscription cleanup. Session IDs are deduplicated and route-less
sessions contribute zero route concurrency. Ephemeral sender-only sessions with
no receiver/subscription remain excluded. The profile rejects an allocation
that cannot leave 20% container headroom after `reserved_memory_bytes` plus
MQTT ingress.

## Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `default_topic` | string | -- | Fallback publish topic used when `OutboundMessage.Address` is empty. The publish topic is never read from `Envelope.Subject`. Validated as an MQTT publish topic at **build time** (wildcards `+`/`#`, the `$share/` prefix, and null bytes are rejected), because it bypasses the runtime address validator — a malformed value would otherwise only fail at first publish, as a broker DISCONNECT that tears down the shared session for every route on it. |
| `qos` | int | `1` | MQTT QoS level (0, 1, or 2) |
| `retain` | bool | `false` | MQTT retain flag |
| `timeout` | duration | `30s` | Per-publish timeout, applied as the **stricter** of this value and the caller's remaining deadline. The session raises the MQTT client's per-packet acknowledgement budget to cover the longest `timeout` of any sender bound to it, so the value you configure is the one that governs a PUBACK wait (see [Packet acknowledgement budget](#packet-acknowledgement-budget)). On a bridge route the dispatcher already wraps every send in the route's `policy.send_timeout` (default 30s), so a `timeout` **shorter** than the remaining route deadline tightens the publish while a **longer** one is capped by the route deadline — it never extends the route ceiling. **Note the coercion asymmetry:** unlike an explicit `qos: 0` or `keep_alive: 0` (honoured as-is), a configured `timeout: 0` is coerced **up** to the `30s` default at build. The 60s Send-time safety-net for a zero timeout is therefore reachable only by a direct library consumer that constructs `SenderOptions` and leaves `Timeout` at `0`, bypassing the factory — via config, a `0` becomes `30s`. See [Resilience Behavior](#resilience-behavior) for the interaction with `policy.send_timeout`. |
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

### Mutual TLS from a credential store

The two halves of mutual TLS are configured independently, and either can come
from a store instead of the filesystem:

- **Server-certificate validation** — the CA that must have signed the broker's
  certificate (`ca_cert_file` / `ca_cert_pem`, or `ca` from the store).
- **Client certificate (mTLS)** — the keypair this bridge presents to the
  broker (`cert_file` + `key_file`, `cert_pem` + `key_pem`, or `cert`/`key`
  from the store).

Pointing at a credential URI supplies both, so no certificate material appears
in the YAML or on disk:

```yaml
options:
  credentials_uri: "pms:///gobridge/prod/mqtt"   # AWS Parameter Store
  session:
    broker_url: "ssl://broker.example.com:8883"  # TLS comes from the SCHEME
    tls:
      enable: true
      insecure_skip_verify: false
```

The stored JSON supplies the material the resolver merges into `tls`:

```json
{
  "username": "bridge",
  "password": "…",
  "ca":   "-----BEGIN CERTIFICATE-----\n…",
  "cert": "-----BEGIN CERTIFICATE-----\n…",
  "key":  "-----BEGIN PRIVATE KEY-----\n…"
}
```

`ca` accepts a single PEM or a list, for a chain or during CA rotation. The
parser also accepts `certPem`/`certificate` and `keyPem`/`privateKey` as
aliases. Rotating any of it in the store is picked up by the credential refresh
path — see [Credential rotation](../credentials-rotation.md).

Two behaviours worth knowing, both fail-closed:

- **TLS is selected by the URL scheme, not `tls.enable`.** A `tcp://` broker URL
  stays cleartext even with `enable: true`. Use `ssl://`, `mqtts://`, `tls://`
  or `wss://`.
- **Credentials are refused over a cleartext connection** unless
  `allow_plaintext_credentials: true` is set explicitly. This is re-checked
  after the store resolves, so a `credentials_uri`-only config cannot smuggle
  secrets onto a `tcp://` broker.

The same pattern works for AMQP 0-9-1, AMQP 1.0 and Azure Service Bus.


## Receiver Options

MQTT receivers have no transport-specific options. Subscriptions are declared
in the `topics[]` array on the `ReceiverDef`, not in the `options` map.

Every entry is validated at **build time** -- the topic filter against the MQTT
v5 filter rules (wildcard placement, `$share/<group>/<filter>` shape, UTF-8, no
null bytes, length) and `qos` against 0/1/2. Both checks are repeated when the
session plan is reconciled, so a plan handed straight to a `Session` by a
library consumer fails the same way. Validating at build matters twice over: a
subscription only reaches the broker when the session manager reconciles the
plan, so a malformed filter would otherwise fail *after* the process had
started serving -- and an out-of-range `qos` would not fail at all. The MQTT
client writes the level as `qos & 0x03`, so `qos: 4` would reach the broker as
`0`: the route would believe it subscribed at-least-once while the broker
delivered at-most-once and never asked for an acknowledgement.

> **Use the factory for production composition (library-consumer note).**
> `Factory.NewReceiver` atomically reserves the session's sole ingress receiver
> and returns `shared.ErrInvalidConfig` for a second call, including calls through
> another factory value or registry alias. The low-level `paho.NewReceiver`
> constructor exists for adapter diagnostics and focused router tests; it bypasses
> factory preflight and must not be used to multiplex production routes onto one
> session. Receiver IDs remain globally unique in bridge configuration.

---
