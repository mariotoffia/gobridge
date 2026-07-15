# MQTT (Paho)

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `mqtt`
**Factory:** `paho.NewFactory(logger)`
**Capabilities:** `stateful_session`, `exclusive_identity`, `dedicated_ingress_session`, `shared_consumer`, `plan_driven_subscriptions`

MQTT requires a session. Each session permits at most one logical ingress
receiver. Multiple senders may still share that session and its TCP connection.
Session mode controls lifecycle and ownership semantics.

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

## Dedicated ingress sessions

Paho has one serialized publish-dispatch worker and one protocol-ack ordering
domain per MQTT session. A blocked processor, destination, DLQ write, or source
settlement therefore pins later publishes on that connection. Per-route queues
cannot isolate this MQTT acknowledgment domain without durable staging and ACK
aggregation.

Paho advertises `CapDedicatedIngressSession`. During `Builder.Plan`, the bridge
rejects a second logical receiver bound to the same session and rejects reuse of
the sole receiver/source binding by multiple route runners. Both checks happen
before it opens any store, transport, or runtime resource and name the conflicting
session, receiver, and routes. Validation is capability-based rather than keyed
to the `mqtt` transport name. Registering Paho under aliases cannot bypass it:
`Factory.NewReceiver` also performs an atomic reservation on the concrete
`Session`. Sender definitions do not consume that reservation and may share a
session.

Use one session (and therefore one broker connection and client ID) per ingress
receiver. Two routes that need independent failure and backpressure boundaries
must name two sessions. This is the supported isolation mechanism; GoBridge does
not add speculative per-route queues or protocol-ACK aggregation.

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

### Clustered shared-subscription identity

A clustered non-Exclusive receiver using `$share/<group>/<topic>` must configure
an effective per-replica client identity. Use `client_id_suffix: hostname` by
default. The hostname is captured once per process, so validation, reload
preflight, and session construction resolve the same effective client ID. Ensure
the deployment gives every live replica a unique hostname (Kubernetes pod names
and ECS task hostnames normally satisfy this).

`client_id_suffix: nonce` is allowed only for Ephemeral sessions. Its random
value is generated once per process, remaining stable across reload checks while
still differing between replicas. Persistent and Exclusive modes reject `nonce`
because a restart would make prior broker state unreachable. Exclusive mode
rejects every suffix: the lease serializes access to one stable client ID shared
by active and standby replicas.

Validation fails closed when a clustered `$share` subscription has no typed
replica-identity capability, an empty strategy, or a durable mode using `nonce`.

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
approximately `lease_ttl`. Production validation rejects an effective
`lease_ttl` below 5s uniformly, before selecting or opening a lease-store
backend. A clustered exclusive route that does **not** pin
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
they are held instead). Otherwise the topic is an **orphan subscription**. Ephemeral sessions preserve
the legacy best-effort concrete-topic cleanup. Persistent/exclusive sessions do
**not** infer a subscription filter from the delivered topic: wildcard and
`$share` filters cannot be reconstructed that way. They use Managed subscription
history (defined below), remove exact filters before handler dispatch, and
recycle the connection so buffered stale QoS 1/2 deliveries remain un-ACKed and
return to the broker. `MQTTRouterUnmatchedDropped` remains the ephemeral orphan
cleanup signal. See
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

## Managed subscription history

A persistent/exclusive MQTT session is restricted to **one distinct canonical
broker URL**. Its exact managed-filter ledger is global to that broker-session
identity, so independent multi-broker failover could apply one broker's filter
history to another and is rejected at build time. Ephemeral sessions may still
use multiple broker URLs.

A persistent/exclusive MQTT session with desired subscriptions requires
`stores.managed_subscriptions` (`sqlite` for one process, `dynamodb` for a
cluster). Startup strongly loads the exact history by the opaque
`DurableSessionIdentity` **before broker activation**. Missing history is not an
empty set: it is an unknown migration state and startup fails below Full. A
store outage has the same fail-closed result; there is no in-memory fallback.

Reconciliation is crash-safe and per-filter: GoBridge `Remember`s every desired
candidate before `SUBSCRIBE`; failed/partial SUBACK candidates remain history;
it computes exact `history - desired`; sends those exact wildcard/shared strings
in `UNSUBSCRIBE`; and `Forget`s only filters whose UNSUBACK reason is success
(`0x00` or `0x11`). Failed, short, or partial acknowledgements stay durable for
retry. While cleanup is slow or failing, every concrete topic matching an exact
pending wildcard/shared history filter remains coverage-protected past
`unmatched_grace`; those deliveries stay un-ACKed. If any stale filter is removed,
GoBridge reconnects before normal handler dispatch and keeps the exact history
durable while it checks the replacement generation. This safely handles the
no-buffer case, but MQTT does **not** portably guarantee that an unacknowledged
shared QoS 1/2 delivery will be redistributed: a broker may pin it to the
persistent client session and replay it to the same ClientID. GoBridge never
ACKs or drops such a replay and never reports convergence/Full. It disconnects,
enters the terminal migration-required path, retains the managed-filter history,
and requires the restore/drain/retry procedure below. Exclusive mode keeps the
lease until natural expiry on this fail-closed path so work cannot continue
under a new owner while accepted work may still settle.

### Removing filters: restore, drain, retry

Before removing persistent/exclusive filters, stop publishers or otherwise drain
traffic covered by the old wildcard/shared filters. A no-buffer cutover removes
the exact filters, recycles, waits one `unmatched_grace` verification window,
then forgets verified history and reaches Full. Initial Exclusive activation
uses one conservative whole-path hard bound rather than the short recurring
reconnect reconcile cap. Paho computes it from every potentially sequential
phase: initial and recycle connection waits, initial/final subscription broker
operations, exact cleanup, bounded ingress quiescence, and both possible replay
verification windows. Nested reconnect-attempt limits are not double-counted.
With the 30s MQTT defaults this conservative bound is 4m, longer than the 45s HA
lease TTL.

The existing lease-renewal loop therefore starts immediately after Acquire and
remains the **only** renewer throughout bounded activation. Successful Renew
keeps the fencing token/current local deadline valid; definitive loss or the
existing renewal-failure step-down cancels activation and disconnects/quiesces
before returning. A parked
activation or failed disconnect is terminal and never releases ownership under
work that may still mutate. This removes backend-dependent timing acceptance and
keeps safe defaults usable, but it does **not** claim the Task 9 failover SLO.
A hard-bound expiry, shorter caller context, or store outage remains uncertainty
and fails closed.

If startup/reconcile reports that managed subscription migration requires the
old configuration, readiness must remain below Full. Do **not** delete/empty the
ledger, use `clean_start`, expire/delete the broker session, or change ClientID;
those shortcuts can discard the pinned delivery. Instead:

1. Stop the failed migration runtime. For Exclusive mode, wait for its retained
   lease to expire before another owner starts.
2. Restore a fresh runtime with the **same broker URL, ClientID, session expiry,
   and `stores.managed_subscriptions` identity**, plus the exact old filters and
   handlers.
3. Let the broker replay the pinned delivery and confirm its normal source
   settlement and downstream durable drain. Keep ingress stopped until the old
   session backlog is empty.
4. Stop that runtime cleanly, reapply the desired configuration, and retry the
   migration. Reach Full only after exact cleanup, recycle, and verification
   complete; then resume publishers and verify a shared peer receives new
   traffic without stale theft.

See the [managed-filter migration runbook](../runbooks/mqtt-managed-subscription-migration.md)
for the operational checklist. GoBridge makes no portable redistribution claim.

**Upgrade baseline is mandatory.** Existing broker sessions predate this ledger,
so GoBridge cannot discover their filters from MQTT. Before enabling this build,
either seed each durable identity with every exact existing filter (including
`sensors/#` and the complete `$share/group/sensors/#` form), or perform a
controlled maintenance migration: stop ingress, exact-UNSUBSCRIBE every old
filter, verify broker backlog/drain, seed an explicit empty baseline, then start.
Never seed empty merely to bypass startup when subscriptions may still exist.

Ordinary live removal/rename/identity change of a persistent/exclusive session is
refused even with `WithAllowDestructiveReload`, because managed filters may
remain. Externally drain, exact-unsubscribe, seed/cut over the new identity, and
only then change configuration.

## Durable identity and live-reload migration

The Supervisor fingerprints the canonical broker set (URL userinfo removed),
effective client ID after suffix resolution, session mode, effective clean-start
behavior, and effective session expiry. A live reload that changes or removes
that identity is refused before the old runtime is stopped or a replacement is
built. Credential rotation, TLS material/path changes, keepalive, reconnect,
reconcile, and other tuning do not change this durable identity.

`WithAllowDestructiveReload` cannot bypass this guard. GoBridge intentionally
does not automate broker-state migration. To change a durable MQTT identity,
operators must externally orchestrate a maintenance cutover: stop new ingress,
drain and verify the old broker backlog, exact-UNSUBSCRIBE every managed filter, remove the old session,
apply the new identity, then resume traffic and verify consumption. In a cluster,
perform this as a coordinated versioned rollout; independent per-process reloads
are unsafe.

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
        receive_maximum: 192
        max_payload_bytes: 262144
        ingress_memory_budget_bytes: 268435456
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
| `broker_urls` | []string | -- | Broker URLs for failover. Ephemeral sessions may use multiple independent URLs. Persistent/exclusive sessions reject more than one distinct canonical URL because one managed-filter history cannot safely span independent broker-session domains. |
| `client_id` | string | -- | MQTT client identifier. **Required** on the effective (merged) session config at build time, together with at least one broker URL (`factory.go:59-66`, `NewSession`); an empty value is accepted at parse time. For scale-out uniqueness from one shared config file, see `client_id_suffix`. |
| `client_id_suffix` | string | -- | Opt-in per-instance uniquifier appended to `client_id`, required for clustered non-Exclusive `$share` consumers. `hostname` appends the process-cached hostname and is preferred for Ephemeral/Persistent sessions. `nonce` appends a process-cached random token and is allowed only for Ephemeral sessions. Unset leaves `client_id` verbatim. Exclusive rejects every suffix because failover resumes one stable shared client ID. |
| `keep_alive` | int | `30` | Keep-alive interval in seconds. Explicit `0` disables the MQTT pinger — half-open-connection detection then rests on TCP keep-alive alone (much slower, and OS-dependent), so a dead-but-open socket can go unnoticed for minutes. The registry/blueprint path defaults to `30`; a direct library consumer that sets `0` should understand this trade-off. |
| `connect_timeout` | duration | `30s` | Bounds the **initial** Start connection await |
| `reconnect_timeout` | duration | `30s` | Bounds each individual (re)connect attempt (TCP dial + TLS + CONNECT/CONNACK). Maps to autopaho `ConnectTimeout`; `0` → autopaho default (10s). |
| `reconcile_timeout` | duration | `30s` (`DefaultReconcileTimeout`) | Bounds **each** broker SUBSCRIBE / UNSUBSCRIBE issued while reconciling the session plan. The reconcile runs on a possibly deadline-less runtime context, so without this an unresponsive broker (SUBACK/UNSUBACK never arrives on a half-open connection) would hang the reconcile — and any startup / hot-reload step awaiting it — indefinitely. This is a **liveness safety bound**: a non-positive value is coerced **up** to `30s` and cannot be disabled. |
| `reconnect_delay` | duration | `10s` (`DefaultReconnectDelay`) | **Base** delay of a jittered exponential reconnect backoff. Starts at `reconnect_delay`, grows 2× (`reconnectBackoffFactor`) per failed attempt, caps at `reconnect_max_delay`, then equal-jitters to `[d/2, d)` to desynchronise a reconnecting fleet (anti thundering-herd). `0` → `10s`. |
| `reconnect_max_delay` | duration | `2m` (`DefaultReconnectMaxDelay`) | Caps the jittered-exponential reconnect envelope. Must be ≥ `reconnect_delay`; a smaller value is clamped up to the base at Start. `0` → `2m`. |
| `clean_start` | bool | `false` | MQTT 5 clean-start flag; consulted only for Persistent/Exclusive sessions |
| `session_expiry_interval` | int | `0` | MQTT 5 session expiry in seconds. For Persistent/Exclusive sessions a `0` is replaced at session creation (`NewSession`) with `86400` (24h) — a literal `0` would give zero offline retention. Ephemeral always uses `0`. |
| `receive_maximum` | int | `0` → **192** (`DefaultReceiveMaximum`) | MQTT 5 Receive Maximum: max in-flight QoS 1/2 messages the broker may send before PUBACKs. `0` is normalized because it is illegal on the wire. The same effective value sizes one reservation shared by the serialized dispatch queue and startup/migration pending entries; those stores cannot each retain a full independent window. A non-zero value is accepted only when the complete ingress byte bound fits `ingress_memory_budget_bytes`; unsafe explicit values are rejected, never clamped. |
| `max_payload_bytes` | int | `0` → **262144** (`DefaultMaxPayloadBytes`) | Maximum inbound application body, in bytes. CONNECT advertises a separate wire Maximum Packet Size of this body limit plus a 128 KiB MQTT v5 metadata allowance. Before adapter enqueue, the callback also enforces the body limit, at most 128 User Properties, and at most 128 KiB encoded topic/properties metadata. A violation terminally recycles the session so QoS 1/2 cannot wedge acknowledgement tracking. Values too large to retain the metadata allowance below the MQTT 256 MiB − 1 packet ceiling are rejected, never clamped. This does not limit outbound publishes. |
| `ingress_memory_budget_bytes` | int | `0` → **268435456** (`DefaultIngressMemoryBudgetBytes`) | Per-session conservative MQTT ingress budget (256 MiB). The bridge validates the full packet/window equation below using the route's effective `max_in_flight` before opening stores or transports. Exact boundary is accepted; one byte over budget and every arithmetic overflow are rejected as invalid config. |
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

### Ingress byte model

Every consumed MQTT session is validated independently:

```text
packet = ceil(maxPacketSize(maxPayloadBytes) * 1.25)
window = receiveMaximum + dispatchCapacity + routeMaxInFlight + 1
bound  = packet * window
```

`dispatchCapacity` is the effective `receiveMaximum`, not a fixed queue size.
One reservation is shared by dispatch and startup/migration pending entries, so
their combined distinct queued packets never exceed that capacity.
`maxPacketSize` is the retained-heap base for an accepted MQTT v5 PUBLISH. It
starts with the separately advertised wire Maximum Packet Size: payload plus a
128 KiB allowance covering the fixed-header byte, worst-case four-byte Remaining
Length encoding, maximal 65,535-byte topic plus its two-byte length, QoS packet
identifier, worst-case properties-length encoding, and bounded property bytes.
It then includes both Paho User Property struct representations (capped at 128)
and a 32 KiB fixed allowance for SDK structures, accepted Envelope header-map
buckets, outbox/queue state, and allocator page/size-class rounding. The 25%
factor covers remaining Go object and slice bookkeeping. The `+1` covers
the packet currently crossing dispatch/handler ownership. Envelope, no-processor
route, and outbox fan-out clones share immutable payload backing; a processor
that calls `SetPayload` owns a new copy. Checked division and overflow guards run
before every addition or multiplication.

The typed parser intentionally keeps zero/unset `receive_maximum`,
`max_payload_bytes`, and `ingress_memory_budget_bytes` as zero through config
clone/parse. This preserves whether an operator supplied a value. The AWS
profile derives omitted values before build; other composition paths normalize
them in `NewSession`. Explicit safe values survive both paths, while explicit
unsafe values fail rather than being reduced.

The defaults (256 KiB payload, Receive Maximum 192, route `max_in_flight` 100)
produce a 263,219,200-byte bound, below the 256 MiB default budget. Raising
payload size, Receive Maximum, or route concurrency may require a larger budget.
Do not tune only the message count.

The AWS file-based profile reserves 25% of the effective Fargate task memory,
divides it across unique consumed MQTT sessions, and derives the largest safe
default Receive Maximum with this same formula. It rejects a profile that cannot
leave 20% container headroom after `reserved_memory_bytes` plus MQTT ingress.
Every Persistent or Exclusive MQTT session referenced by a declared sender
consumes one deduplicated allocation even when no route references that sender,
because the session is still built and resumed durable broker state may deliver
stale backlog before managed-subscription cleanup; its route concurrency
contribution is zero. Ephemeral sender-only sessions are excluded.

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

### Bounded recovery from an unsettled delivery

A successful `Delivery.Retry` on a QoS 1/2 delivery has transport-specific
semantics because MQTT has no per-message NACK. On a **Persistent** or
**Exclusive** session, Retry leaves the PUBLISH protocol-unsettled and requests
one connection recycle. Ack and Retry are mutually exclusive and idempotent: a
Retry that wins can never be followed by a protocol Ack for that delivery.
QoS 0 and every Ephemeral-session Retry remain `ErrNotSupported` because a
reconnect cannot redeliver them safely.

Recovery applies these safety bounds without introducing a recovery-specific
config knob:

- readiness drops below Full synchronously when Retry queues recovery. Queueing
  immediately arms Session Present enforcement: any subsequent ConnectionUp with
  `Session Present=false` irreversibly fails that recovery, even before the
  worker owns the gate. The request still carries no active-attempt or target-
  epoch evidence; the worker publishes those only after acquiring the session
  gate, so an ordinary reconcile that wins first cannot validate or abort queued
  recovery;
- concurrent requests coalesce into one recycle;
- the router stops accepting new callbacks, lets other accepted settlements
  drain for at most **5 seconds**, then disconnects even if that drain remains
  incomplete;
- completed recovery attempts are spaced by at least **30 seconds**, using the
  session clock, to prevent a DLQ-outage reconnect storm;
- ordinary reconciliation, credential/TLS reload, managed cleanup, orphan
  cleanup, and settlement recovery use one context-aware session serialization
  gate. Every public entry acquires it with its own context; private reload and
  reconcile-under-gate helpers never reacquire it, so there is no nested
  serialization wait or ABBA lock order. A caller cancelled while queued leaves
  promptly;
- one hard deadline covers waiting for that gate, the settlement drain,
  disconnect, reconnect, and replacement-generation reconcile. It reuses the
  conservative post-acquire activation timing derived from `connect_timeout`,
  `reconcile_timeout`, and `unmatched_grace`; there is no duplicate recovery
  timeout setting;
- the rebuild preserves `client_id` and session expiry while forcing
  `clean_start=false`;
- CONNACK must report **Session Present**. If it does not, the broker cannot
  prove the unsettled packet survived. Session Present evidence is stamped with
  the exact connection epoch; recovery captures its target epoch after reconnect,
  and reconciliation rejects evidence from any older or newer epoch. Session
  Present alone is not completion: readiness stays degraded until exact-epoch
  replacement reconciliation succeeds within the same deadline;
- every queued or active recovery failure (gate timeout/cancellation, bounded
  drain, disconnect/reconnect, Session Present, or reconcile) enters one
  idempotent terminal transition. It clears pending attempt state, latches a
  permanent error, quiesces ingress, disconnects the generation within the
  activation bound, emits one terminal SessionError, then closes the lifecycle
  event channel. One generation-scoped drain state (`not-started`, `in-progress`,
  or `finished`) gives exactly one owner the settlement barrier: terminal teardown
  starts it only from `not-started`, joins the same completion signal while it is
  `in-progress`, and disconnects immediately once it is `finished` (success or
  timeout). Session Present failure before or during drain therefore cannot start
  a second drain or signal the manager before the shared barrier/bounded abort.
  The manager therefore tears
  down before releasing an exclusive
  lease; its supervisor retries once, and the existing single-use contract then
  escalates `ErrSessionUnrecoverable` for orchestrator replacement. Future Retry,
  Reconcile, credential, and Start calls return the terminal error rather than
  reactivating the dead Session instance.

The adapter tracks every current-connection QoS 1/2 packet from receipt until a
successful protocol Ack or connection-epoch change. Deep health exposes
`unsettled_count`, `oldest_unsettled_age`, `receive_window_utilization`, and
`recovery_recycle_count`. The corresponding metrics are:

| Metric | Kind/unit | Meaning |
|---|---|---|
| `MQTTUnsettled` | gauge, packets | Current-epoch QoS 1/2 packets awaiting protocol settlement. |
| `MQTTOldestUnsettledAge` | gauge, seconds | Age of the oldest current-epoch unsettled packet. |
| `MQTTReceiveWindowUtilization` | gauge, ratio | `unsettled_count / receive_maximum`; sustained values near 1 indicate ingress is close to flow-control exhaustion. |
| `MQTTSessionRecoveryRecycle` | counter | Actual recycle attempts started after acquiring the session gate. Queue timeout/cancellation before recycle does not increment. |

All four metrics use only the existing `session_id` tag. Message IDs, topics,
and failure reasons are deliberately not dimensions, so cardinality remains
bounded.

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
- **Ingress properties are session-owned copies.** The router converts incoming
  MQTT Properties (User properties, CorrelationData, ContentType, etc.) into an
  owned envelope before dispatch. Config-driven composition binds at most one
  receiver to that session, so no route shares its dispatch or acknowledgment domain.
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

- The **dispatch queue** holds up to the effective `receive_maximum` (default
  **192**) items. When it
  is full under a flood, a **QoS 0** publish is dropped (`MQTTRouterDropped`,
  logged) because QoS 0 carries no delivery contract; a **QoS 1/2** publish
  blocks until a slot drains (bounded by the broker's Receive-Maximum window), so
  at-least-once is preserved as broker backpressure.
- The **pre-registration pending buffer** absorbs the CONNACK backlog that
  arrives before receivers register (see [Session Modes](#session-modes)). It has
  two independent bounds applied asymmetrically by QoS: an entry-count cap sized
  to `receive_maximum` (default **192**) and a **64 MiB** payload ceiling
  (`defaultPendingBytesLimit`). The byte ceiling governs **QoS 0 only**. A QoS 0
  publish over either cap is dropped (`MQTTRouterDropped`) — it carries no
  delivery contract. A **QoS 1/2** publish is never dropped for the byte ceiling:
  it evicts the oldest QoS 0 entry to reclaim memory and buffers regardless,
  bounded by the count cap. QoS 1/2 memory needs no byte cap because the broker's
  Receive-Maximum flow control never delivers more than `receive_maximum` un-acked
  QoS 1/2 at once. The complete packet/window allocation is covered by the
  validated ingress byte model above. The single path that drops a QoS 1/2 publish is the count cap hit
  with no QoS 0 left to evict — reachable only when a broker exceeds the Receive
  Maximum it was granted (a protocol violation). That publish is acked-and-dropped
  (dropping-with-ack keeps paho's in-order ack stream draining) and counted on
  `MQTTRouterOverflowDropped`, so any non-zero value points at a broker bug, not
  operator mis-sizing. Publishes held in the buffer count on `MQTTRouterBuffered`.

The dispatch queue, broker receive window, route concurrency, current packet,
whole-packet ceiling, and runtime bookkeeping are all included in the validated
byte bound. A non-compliant broker can still put one decoded packet in the SDK
before the callback sees it, but an oversize body is rejected before the adapter
makes its own copy or enqueues it; QoS 1/2 remains unacknowledged, preserving
at-least-once semantics.

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

> **Use the factory for production composition (library-consumer note).**
> `Factory.NewReceiver` atomically reserves the session's sole ingress receiver
> and returns `shared.ErrInvalidConfig` for a second call, including calls through
> another factory value or registry alias. The low-level `paho.NewReceiver`
> constructor exists for adapter diagnostics and focused router tests; it bypasses
> factory preflight and must not be used to multiplex production routes onto one
> session. Receiver IDs remain globally unique in bridge configuration.

---
