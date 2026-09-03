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
