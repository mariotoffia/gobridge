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

### Deployment identity

Persistent mode keys the broker's durable session — its subscriptions **and
queued offline QoS 1/2 messages** — to the effective `client_id`. A
`client_id_suffix: hostname` is therefore safe **only where the hostname is
stable across restarts of the same replica**:

| Orchestrator shape | Hostname across a restart/rollout | Persistent + `hostname` suffix |
|---|---|---|
| Kubernetes **StatefulSet**, VM, bare metal | stable (`pod-0` stays `pod-0`) | **Safe** — the replica resumes its own broker session. |
| Kubernetes **Deployment**, ECS service | new pod/task name every rollout | **Unsafe** — every rollout mints a new `client_id`, so the previous broker session (with its queued QoS 1/2) is ORPHANED. No instance can drain it; it expires silently after `session_expiry_interval` (default 24h) — **loss by timeout, invisible to the bridge**. |

The bridge cannot detect its orchestrator, so a startup warning is not an
admission boundary. `Factory.NewSession` therefore **rejects**
`session_mode: persistent` combined with `client_id_suffix: hostname` (returns
`INVALID_CONFIG`) unless the operator explicitly asserts a stable-host profile
with `assert_stable_client_identity: true`. The assertion is the
operator vouching for StatefulSet/VM identity — it does **not** make a
Deployment/ECS service safe; an admitted session still warns it is unsafe there.
On a Deployment/ECS service use one of the safe shapes instead:

- **StatefulSet** (stable hostnames) when persistent per-replica sessions are
  genuinely wanted — set `assert_stable_client_identity: true` to admit it;
- **`session_mode: exclusive`** (one stable shared `client_id` + lease): the
  surviving/next instance resumes the queue on takeover;
- **Ephemeral + `$share`** when per-replica offline retention is not needed —
  the broker load-balances and nothing is queued per dead replica.

**Broker-side HA is single-endpoint for durable modes.** Persistent and
exclusive sessions reject more than one canonical `broker_urls` entry: the
durable managed-filter history is keyed to a single broker-session domain, so
broker-level HA for durable MQTT sessions must come from a broker cluster
behind one stable endpoint (DNS name / load balancer), never from client-side
URL lists. Multi-URL client-side failover is available to ephemeral sessions
only.

*Canonical* means the endpoint actually dialled, not the URL as written: scheme
aliases collapse (`tcp`/`mqtt`, and `ssl`/`tls`/`mqtts`/`mqtt+ssl`/`tcps`), an
omitted port becomes the family default, host case, userinfo and fragment are
ignored, and path/query count only for `ws`/`wss`. Two durable sessions spelled
differently but reaching one endpoint are therefore rejected as duplicate
identities at startup, instead of starting and disconnecting each other on their
shared `client_id`.

> **Upgrade note — durable session identity.** The canonical endpoint feeds the
> fingerprint that keys managed-subscription storage. A URL written as
> `tcp://host:1883`, `ssl://host:8883`, `ws://host:80/path` or
> `wss://host:443/path` keeps the identity it had before, so its stored history
> carries forward untouched. Any other spelling — an alias scheme, or an omitted
> port — resolves to a new fingerprint, and that session starts with empty
> managed-subscription history. Rewrite such URLs into the canonical spelling
> **before** upgrading, or treat the change as identity-incompatible and deploy
> it by whole-cohort replacement (see `docs/cluster/operating.md`).

**`deployment_mode: standalone` is a per-process assertion.** Two replicas
each declaring `standalone` with process-local lease stores each believe they
own every exclusive session — real split-brain consumption. The bridge logs a
prominent `SPLIT-BRAIN RISK` warning for any exclusive session on a local
lease store; alert on that log line, and enforce `replicas: 1` at the
orchestrator for standalone deployments. `deployment_mode: clustered`
hard-fails on non-distributed stores instead.

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

**Failover timing.** A clustered exclusive route that leaves lease timing
unset uses the 45s HA lease cadence, but that cadence is not an end-to-end SLO.
The worst-case failover budget is:

```
budget = lease TTL
       + acquire-poll boundaries
       + lease-store observation call budget
       + transport post-takeover activation
       + startup_allowance
```

where the MQTT **post-takeover activation** term alone is
`2×connect_timeout + 4×reconcile_timeout + 2×unmatched_grace`
= **240s with the shipped defaults** (30s each). With the auto-selected
clustered HA profile (lease TTL 45s, 5s acquire poll, 3s renew-call timeout)
the default worst case is therefore **≈336s**, and with standalone lease
defaults (360s TTL, whose longer TTL also inflates the lease-store
observation call budget) the computed worst case is **≈1097s (~18 minutes)**. A ~50s worst case is
reachable with explicit tuning — e.g. `lease_ttl: 15s`, `connect_timeout: 3s`,
`reconcile_timeout: 2s`, `unmatched_grace: 2s` — the controlling keys are
`lease_ttl`, `acquire_poll_interval`, `renew_call_timeout`, `max_renew_fails`,
`startup_allowance`, plus the MQTT `connect_timeout`, `reconcile_timeout`, and
`unmatched_grace`.

Every build **logs this computed budget** for each exclusive session that
declares no `failover_slo` (look for `worst-case failover budget` at startup).
Declare `routes[].session.failover_slo` to turn the disclosure into a
contract: the build then **fails** when lease TTL, jittered acquire polling,
renew-call timeout, the complete conservative Paho post-takeover activation
bound, and startup allowance cannot meet the target. Validation is necessary but warm
and cold failure-detection-to-`ServiceLevelFull` measurements are required before
publishing any latency claim. The activation bound includes initial connect,
managed cleanup/replay, recycle/reconnect, final reconciliation, and grace
windows exactly once. The practical bound additionally includes pod-restart
latency when the lease-losing instance must recycle (the single-use session
restart policy below); budget it via `startup_allowance`. See
[Scenario 8 — Failover SLO Validation](../scenarios/08-clustered-exclusive-sessions.md#failover-slo-validation).

**Broker-path failover (node-local outage).** The failover budget above covers
lease/owner/process loss. It does **not** cover the case where the active
exclusive owner **alone** loses its network path or authorization to the broker
while the lease store stays reachable: renewals keep succeeding, so the owner
holds the lease and Paho reconnects forever, and a healthy standby (blocked in
acquire-before-connect) can never take over — cluster availability stays down
indefinitely (finding CLUSTER-2). Broker-path failover is therefore **opt-in**:
set `routes[].session.broker_health_step_down` to a positive duration and an
active owner whose broker path stays **non-converged** (disconnected, or
connected but not re-subscribed) that long voluntarily steps down — releasing the
lease so a standby seizes it — and emits `BrokerHealthStepDown`. Leave it unset
(the default) when the broker is fronted by a single HA endpoint reachable from
every node, because a *globally* unreachable broker would otherwise churn the
lease between nodes that all fail to connect. When set, it **extends the
worst-case failover budget by up to `broker_health_step_down`**, so keep it
comfortably above normal reconnect+reconcile time and account for it against any
declared `failover_slo`. Alert on `BrokerHealthStepDown` — a non-zero rate means
a node is losing its broker path.

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

### Reload semantics: a controlled restart, not a hitless reload

Every MQTT-containing configuration change takes the serialized
prepare-commit swap: **all MQTT sessions disconnect** (drain ≤ the configured
`drain_timeout`, default 30s), the new runtime is built, dialed, and
reconciled. During that window:

- **QoS 1/2 on `clean_start=false` (persistent/exclusive) sessions**: queued
  broker-side and replayed after reconnect — **no loss**, possible duplicates
  (at-least-once);
- **QoS 0 on any session**: lost for the duration of the window (no delivery
  contract);
- **ephemeral sessions**: everything published in the window is lost (the
  broker discards the session at disconnect).

Plan reloads accordingly: batch config changes (or enable a
debounced/windowed reconfig strategy — the default applies each change
directly, so N rapid writes are N windows), and schedule reloads for
ephemeral/QoS 0 traffic like any other restart.

**Reload success means "applied", not "converged".** The swap reports success
once the new runtime is built and started; MQTT dials and reconciles in
background goroutines, so a syntactically-valid-but-broker-invalid config
(ACL-denied topic, rotated-away credentials) commits as a successful reload
while the transport is down. The supervisor's post-swap convergence watch
closes the gap: it observes the new runtime until sessions reach
`LevelSubscribed`, and past the transport's declared activation budget it
flips `ConfigDegraded` to 1 with an `applied but ... not converged` reason in
deep health (`/api/v1/monitor/deephealth` → `config_watch.reason`), clearing
automatically if the sessions later converge. Operator rule: after every
reload, verify session health (or watch `ConfigDegraded`) — the reload
success signal alone is insufficient. Remediation for a non-converging
config is a revert (see `docs/runbooks/config-rollback.md`).

Note also: one permanently rejected subscription (broker denies a filter, or
grants a lower QoS) fails the whole reconcile; on an exclusive session the
lease is released and supervision retries forever at the 30s backoff cap —
connect → subscribe → reject → disconnect, indefinitely, with readiness below
Full. There is deliberately no per-topic quarantine (a partial route set is
never silently served). See `docs/runbooks/mqtt-suback-rejection-flap.md`.

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


---

## Egress wire limits

A publish is measured against two ceilings **before any byte reaches the
socket**. Both refusals return a permanent rejection — the route DLQs the
message rather than retrying it — and count `MQTTEgressRejected`. Any non-zero
value on that counter means a producer or route is generating messages this
broker cannot accept.

**Broker Maximum Packet Size.** The broker grants a ceiling in its CONNACK
(MQTT v5 §3.2.2.3.6); an absent property means no limit. The adapter captures
it per connection — autopaho reconnects underneath the session, and a resumed
or relocated broker can grant a different value — and rejects any PUBLISH whose
encoded size exceeds it. Writing an over-limit packet instead is answered with a
broker DISCONNECT: QoS 1/2 completion becomes ambiguous, QoS 0 has already
reported local success, and every retry recycles the session.

**Field limits.** Every length-prefixed MQTT v5 field — topic, content type,
response topic, Correlation Data, and each User Property key and value — is
capped at 65,535 bytes by its two-byte length prefix. The Paho SDK slices a
longer value and writes the shortened form *without an error*, so the broker
would acknowledge metadata that differs from the source: a cut idempotency key
stops deduplicating, a cut tenant id mis-attributes, a cut correlation id breaks
the reply path, and a cut multi-byte rune is not valid UTF-8 on the wire. The
adapter refuses such a publish instead of corrupting it.

**UTF-8 validity.** The same string fields must be well-formed UTF-8 and free of
U+0000 (MQTT v5 §1.5.4). A header carrying invalid UTF-8 — from a processor, or
from a source transport that does not enforce it — would otherwise leave as a
malformed packet and the broker would answer with a DISCONNECT, recycling the
session for every message that reproduces it. Correlation Data is exempt: it is
binary on the wire, so any byte sequence is legal and round-trips intact.

**Message expiry.** An envelope carrying an expiry is *always* published with a
Message Expiry Interval. The route decides whether to send; by the time the
packet is built the remaining TTL can already have run out, and MQTT v5 has no
"already expired" encoding (the interval is whole seconds, and zero means "no
expiry"). A non-positive or sub-second remainder therefore clamps to **one
second** rather than omitting the property, which would leave the broker holding
the message for a queued subscriber with no expiry at all.

## Publish namespaces

A publish topic is checked for the MQTT v5 structural rules — non-empty, at most
65,535 bytes, no `+` or `#` wildcard, no null byte — and nothing else. In
particular a leading `$` is **allowed**: MQTT v5 §4.7.2 reserves that prefix for
the *server* to define rather than making it malformed, and real brokers define
legal write namespaces there, AWS IoT's `$aws/rules/<rule>` republish target
being the common one. Refusing the whole prefix terminalized those messages
inside the bridge before the broker ever saw them. `$share/` stays refused: it
names a subscription group, so it can never be a publish destination.

Whether a particular `$` namespace accepts a write is the broker's
authorization decision; its refusal arrives as a PUBACK reason code and is
classified as described next, so a denial stays visible and terminal. A
deployment that must confine its routes to particular namespaces expresses that
as broker-side ACL policy — the adapter carries no namespace allowlist.

## Broker acknowledgements and error classification

A rejected SUBACK, UNSUBACK or PUBACK reason code is the broker telling the
bridge *why* it refused. The MQTT client returns that acknowledgement together
with a generic error for any reason code of `0x80` or higher, so the adapter
classifies the **reason code first** and falls back to the client's error only
when no acknowledgement arrived at all.

This is what keeps a denial permanent. Reason `0x87` (*Not authorized*) on a
publish classifies as `FORBIDDEN` — dead-lettered immediately, cause intact.
Read only as a generic error it would classify as `UNAVAILABLE`, so the route
would retry a message the broker will never accept until the replay budget ran
out, then dead-letter it as `max_retries` with the real cause lost. A partially
accepted SUBACK behaves the same way: the granted filters are recorded as
broker-observed state even though the call as a whole failed.

Configuration failures are classified `INVALID_CONFIG`, not `INVALID_PAYLOAD`.
The two differ in class (`permanent` versus `rejected`), and reporting a
build-time configuration failure as a payload rejection makes automation and
metrics attribute a deployment error to message traffic. Everything the factory
refuses — a missing `client_id`, an empty `broker_urls`, an invalid
`client_id_suffix`, an unpublishable `default_topic`, an out-of-range `qos`, a
malformed subscription filter, cleartext credentials on a non-TLS broker — is
`INVALID_CONFIG`. `INVALID_PAYLOAD` stays reserved for a rejected message.

## Dialing through a proxy

`ALL_PROXY` (or `all_proxy`) routes broker dials through a SOCKS5 proxy, and
`NO_PROXY` (or `no_proxy`) exempts hosts from it. Both spellings are read on
every dial with the **uppercase** taking precedence — the same rule
`golang.org/x/net/proxy` and `net/http` use, so no two resolvers in the process
can disagree about which proxy is in force.

Two behaviours are deliberate and differ from `proxy.FromEnvironment`:

- **An unusable value fails the dial.** An unparseable URL or a scheme that
  cannot be built (for example `http://`, which is not a SOCKS proxy) returns an
  error instead of quietly dialing direct. A proxy is a network-control
  boundary; silently bypassing it is worse than a loud connect failure.
- **`ALL_PROXY=direct`** (or `direct://`) is an explicit opt-out, for a
  container where the variable is set for other tools but the broker must be
  reached without it.

TLS broker connections derive the certificate `ServerName` from the broker URL
host on **both** the direct and the proxied path, so an `ssl://` connection
through a proxy verifies the broker's identity exactly as a direct one does.
Previously the proxied path set no name at all, leaving a certificate-validating
proxied connection unable to verify the broker.

---

## Reference

This page covers sessions and a worked example. The rest of the MQTT
documentation is split by what you are looking for:

| Page | Covers |
|---|---|
| [MQTT options](mqtt-options.md) | Session, sender and receiver options; credential URIs; mutual TLS from a credential store |
| [MQTT behaviour](mqtt-behavior.md) | Settlement semantics, resilience and reconnection, backpressure, shared subscriptions, ingress headers |
