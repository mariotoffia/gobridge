# MQTT durable session state

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
persistent client session and replay it to the same ClientID. What GoBridge
does with such a replay depends on whether the runtime has a dead-letter store
(`stores.dlq`):

- **With a dead-letter store**, GoBridge writes the replay to it with error
  code `SUBSCRIPTION_REMOVED`, acknowledges it only after the write is durable,
  forgets the filter, and converges. See [Removing filters](#removing-filters).
- **Without one**, GoBridge never ACKs or drops such a replay and never reports
  convergence/Full. It disconnects, enters the terminal migration-required
  path, retains the managed-filter history, and requires the restore/drain/retry
  procedure below. Exclusive mode keeps the lease until natural expiry on this
  fail-closed path so work cannot continue under a new owner while accepted
  work may still settle.

**On MQTT 3.1.1** an UNSUBACK carries no reason codes, so the session reports
Success for every filter of an UNSUBSCRIBE. Managed cleanup therefore always
takes its connection-recycle path: one extra reconnect per cleanup. A broker
that refuses an UNSUBSCRIBE anyway (Mosquitto dynamic-security ACLs can) looks
like success, and GoBridge forgets a filter the broker still holds. On 3.1.1
the broker must permit UNSUBSCRIBE for every filter the session subscribes.

### Removing filters

Before removing persistent/exclusive filters, stop publishers or otherwise drain
traffic covered by the old wildcard/shared filters. A no-buffer cutover removes
the exact filters, recycles, waits one `unmatched_grace` verification window,
then forgets verified history and reaches Full. Initial Exclusive activation
uses one conservative whole-path hard bound rather than the short recurring
reconnect reconcile cap. Paho computes it from every potentially sequential
phase: initial and recycle connection waits, initial/final subscription broker
operations, exact cleanup, bounded ingress quiescence, both possible replay
verification windows, and the dead-letter write budget of each window. Nested
reconnect-attempt limits are not double-counted. With the 30s MQTT defaults
this conservative bound is 5m, longer than the 45s HA lease TTL.

The existing lease-renewal loop therefore starts immediately after Acquire and
remains the **only** renewer throughout bounded activation. Successful Renew
keeps the fencing token/current local deadline valid; definitive loss or the
existing renewal-failure step-down cancels activation and disconnects/quiesces
before returning. A parked
activation or failed disconnect is terminal and never releases ownership under
work that may still mutate. This removes backend-dependent timing acceptance and
keeps safe defaults usable, but it does **not** claim a failover SLO.
A hard-bound expiry, shorter caller context, or store outage remains uncertainty
and fails closed.

#### Deliveries held for a removed filter

MQTT 5 has no way to hand a delivery back to the broker: a PUBACK with an error
reason code ends the delivery like a successful one. A delivery the broker
still holds for a removed filter — sent before the disconnect and never
acknowledged, or queued while the bridge was disconnected — is resent after
every reconnect and occupies one Receive Maximum slot until it is acknowledged.
When enough are held, the broker stops sending anything to the session.

With a dead-letter store, the session settles each such delivery during the
cleanup:

1. It writes the delivery to the dead-letter store. The record has error code
   `SUBSCRIPTION_REMOVED`, category `permanent`, reason `subscription removed`,
   the session ID, the route ID of the session's single ingress route (empty
   when no single route rides on the session), the session ID again as its
   source ID, and the removed filter, exactly as configured (for example
   `$share/group/sensors/#`), as its address.
2. It acknowledges the delivery only after the write is durable.
3. It keeps doing this until the current connection's replay-grace window
   (`unmatched_grace`, counted from the connection coming up) ends; a delivery
   does not restart the window. Then it forgets the filter and reaches Full.

A delivery whose topic a still-desired filter also covers is not dead-lettered.
That happens when a replacement overlaps the removed filter, for example
`$share/old/a/#` replaced by `$share/new/a/#`, or `a/#` replaced by `a/b`:
the delivery is live traffic for the new filter, so the session keeps it and
delivers it to the route once the removed filter is forgotten. This also
applies without a dead-letter store; previously such an overlap failed closed.

If a write fails, the delivery stays unacknowledged and the filter stays in the
managed history. The reconcile fails with a transient `UNAVAILABLE` error, the
session manager retries it with backoff, and the process and every other route
keep running. The dead-letter writes of one replay-verification pass share one
`reconcile_timeout`, counted from the first write; a write still running when
it runs out fails the reconcile the same way. A retry can write the same delivery
again if its acknowledgement failed after the write; nothing is lost.

Afterwards, inspect the `SUBSCRIPTION_REMOVED` records, then redrive or purge
them. See the [managed-filter migration runbook](../runbooks/mqtt-managed-subscription-migration.md#dead-lettered-deliveries-inspect-then-redrive-or-purge).

#### Without a dead-letter store: restore, drain, retry

If startup/reconcile reports that managed subscription migration requires the
old configuration, readiness must remain below Full. Do **not** delete/empty the
ledger, use `clean_start`, expire/delete the broker session, or change ClientID;
those shortcuts can discard the pinned delivery. Instead:

1. Stop the failed migration runtime. For Exclusive mode, wait for its retained
   lease to expire before another owner starts.
2. Restore a fresh runtime with the **same broker URL, ClientID, session expiry,
   and `stores.managed_subscriptions` identity** (on MQTT 3.1.1, also the same
   `protocol_version`), plus the exact old filters and handlers.
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

Three composition roots seed the same row: the AWS profile from
`ManagedSubscriptionBaselines` at deploy time, the reference binary (and the
[Kubernetes profile](../../deployment/kubernetes/README.md)'s init container)
from `gobridge -config bridge.yaml -seed-managed-subscriptions <session-id>`
(an empty baseline) or `-seed-managed-subscriptions '<session-id>=<filter>,<filter>'`
(the exact existing filters), and any custom root from
`Builder.SeedManagedSubscriptionBaselines`. All three are idempotent: an
established baseline is kept and listed filters are added to it.

Ordinary live removal/rename/identity change of a persistent/exclusive session is
refused even with `WithAllowDestructiveReload`, because managed filters may
remain. Externally drain, exact-unsubscribe, seed/cut over the new identity, and
only then change configuration.

## Durable identity and live-reload migration

The Supervisor and the AWS runtime fingerprint the canonical broker set (URL
userinfo removed), effective client ID after suffix resolution, session mode,
effective clean-start behavior, and effective session expiry. A live reload that
changes or removes that identity is refused before the old runtime is stopped or
a replacement is built. Both run the same comparison; a custom composition root
calls `bridge.DurableSessionIdentityChanged` before it reloads. Credential
rotation, TLS material/path changes, keepalive, reconnect, reconcile, and other
tuning do not change this durable identity.

On MQTT 3.1.1 (`protocol_version: v3.1.1`) the fingerprint also includes the
protocol version. A broker need not resume a session that was created over the
other version (AWS IoT Core does not), so switching a Persistent or Exclusive
session between `v5` and `v3.1.1` changes its durable identity: a live reload
refuses it, and it needs the maintenance cutover below and the
[managed-filter migration](../runbooks/mqtt-managed-subscription-migration.md).
Drain the backlog before you switch. On `v5` the fingerprint is the one it was
before MQTT 3.1.1 support existed, so existing sessions keep their stored
history. The startup check that rejects two durable sessions with one client ID
on one broker ignores the protocol version: they collide whatever their
versions.

On MQTT 3.1.1 the session expiry is never sent, so the broker decides how long
a session lives after the bridge disconnects (broker defaults: Mosquitto never
expires it, EMQX after 2 h, AWS IoT after 1 h). A non-zero
`session_expiry_interval` is rejected on 3.1.1, so the fingerprint always holds
the local 86400-second default.

`WithAllowDestructiveReload` cannot bypass this guard. GoBridge intentionally
does not automate broker-state migration. To change a durable MQTT identity,
operators must externally orchestrate a maintenance cutover: stop new ingress,
drain and verify the old broker backlog, exact-UNSUBSCRIBE every managed filter, remove the old session,
apply the new identity, then resume traffic and verify consumption. A running
process refuses the new identity, so it is applied by a restart: on AWS, store
the new version and restart the task
([durable MQTT session identity](../aws-deployment/config-reload.md#durable-mqtt-session-identity)).
In a cluster, perform this as a coordinated versioned rollout; independent
per-process reloads are unsafe.

### Reload semantics: a controlled restart of what changed, not a hitless reload

A change confined to sessions, receivers, senders, bindings and routes reloads
in place ([ADR 0018](../adr/0018-reload-in-place-by-unit.md)): only the MQTT
sessions in the reload units the change touches disconnect. A session whose
unit is unchanged stays connected. The changed units are serialized, because
MQTT claims an exclusive client ID: the old sessions stop (drain ≤ the
configured `drain_timeout`, default 30s) before their replacements are built,
dialed and reconciled. A bridge-wide change (`bridge`, `stores`,
`config_watch`, `http`) still takes the full prepare-commit swap, and then
**all MQTT sessions disconnect**. An MQTT session's options depend only on its
own configuration (`receive_maximum` is the count it sets, or 192), so adding
or removing one MQTT session replaces only that session's reload unit; every
other MQTT session stays connected (see
[keeping MQTT tenants connected](../aws-deployment/config-reload.md#keeping-mqtt-tenants-connected)).
For every session that disconnects, during the window:

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
once the new runtime, or the replaced units, are built and started; MQTT
dials and reconciles in background goroutines, so a
syntactically-valid-but-broker-invalid config
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

Note also: one permanently rejected subscription (broker denies a filter)
fails the whole reconcile; on an exclusive session the lease is released and
supervision retries forever at the 30s backoff cap — connect → subscribe →
reject → disconnect, indefinitely, with readiness below Full. There is
deliberately no per-topic quarantine (a partial route set is never silently
served). See `docs/runbooks/mqtt-suback-rejection-flap.md`. A filter the
broker grants at a lower QoS does not fail the reconcile; see
[QoS downgrade](mqtt-behavior.md#qos-downgrade).

## Retained messages on a resumed MQTT 3.1.1 session

An [MQTT 3.1.1](mqtt-311.md) session has no Retain Handling, so the broker
sends a filter's retained messages again every time the session subscribes to
it (MQTT 3.1.1 §3.8.4). An MQTT 5 session asks the broker not to, with Retain
Handling 1. A 3.1.1 session avoids it by not sending the SUBSCRIBE.

When a Persistent or Exclusive session reconnects and the broker answers
Session Present 1, the broker still holds the session, and its subscriptions
with it (MQTT 3.1.1 §3.2.2.2). The session then keeps its record of the
subscriptions the broker confirmed before the connection dropped. The reconcile
that follows sends SUBSCRIBE only for a filter that is new or whose requested
QoS changed, and still unsubscribes a removed filter. Readiness waits for that
reconcile, as on every connection. A Reload that succeeds (a credential
rotation that needs a new connection, a managed-subscription cleanup, a
settlement recovery) reconnects the same way.

The session subscribes a filter again, and the broker replays its retained
messages, when:

- the broker answers Session Present 0: it lost the session, or never had one
  (`MQTTSessionResumeLost` counts a loss);
- the process starts: a new session has no record, so its first connection
  subscribes every filter;
- the filter was granted below its requested QoS: every reconnect re-checks the
  grant, and so does each confirmation and re-check SUBSCRIBE (see
  [QoS downgrade](mqtt-behavior.md#qos-downgrade));
- the filter's last SUBSCRIBE or UNSUBSCRIBE got no acknowledgement before the
  connection dropped, so the broker may or may not have applied it;
- the session gave up the broker session: an exclusive reconcile failed and
  released the lease, the session failed for good, or a Reload failed.

An Ephemeral session starts clean on every connect, so it never resumes and
always subscribes every filter. Session Present is trusted only for one broker:
a Persistent or Exclusive session may use only one canonical broker URL.

**What this relies on.** The broker keeps a session's subscriptions for as long
as it answers Session Present 1, as MQTT 3.1.1 requires. A broker that answers
Session Present 1 but has lost a subscription leaves that filter silent until
the session next subscribes it. No other client changes the session's
subscriptions: give each instance its own `client_id`, or use an exclusive
lease, as for any durable session. Only Mosquitto is tested.

The decision and the hazards it guards against are in
[ADR 0023](../adr/0023-keep-resumed-mqtt-311-subscriptions.md).
