# 0011 — Cluster client-ID uniqueness enforcement

Status: accepted
Date: 2026-07-13
Deciders: GoBridge core
Implemented: commit 4d8d76d (2026-07-13)

## Context

MQTT requires a client identifier to be unique per broker connection. When two
live instances connect with the **same** `client_id`, the broker enforces
uniqueness the only way the protocol allows: it disconnects the incumbent with a
Session-Taken-Over (`0x8E`), the kicked instance reconnects and kicks the other,
and the two mutually evict each other in a tight loop — a self-inflicted denial
of service that also churns subscriptions and in-flight state.

Two deployment shapes collide with this:

- **Scale-out replicas sharing one config** (a ConfigMap, an ECS task
  definition) all read the *same* `client_id`. For `$share` fan-out this is a
  misconfiguration — every replica must be unique — but the shared config file
  makes uniqueness awkward to express per replica.
- **Exclusive (single-active) sessions**, by contrast, *must* share a stable
  `client_id` across instances: failover works by a standby claiming the same
  identity behind a lease, so a per-instance-unique id would strand the broker's
  queued offline state on the dead instance's id (see
  [scenario 08](../scenarios/08-clustered-exclusive-sessions.md)).

So the requirement is not "always unique" — it is "unique for scale-out, stable
for exclusive", and the bridge must make the right one easy and the wrong one
loud.

## Decision

**Provide opt-in per-replica uniquification, forbid it exactly where a stable
shared id is required, and detect a collision loudly and dampen its storm — but
do not hard-mandate a suffix at build.**

- **Opt-in `client_id_suffix` uniquifier.** `resolveClientIDSuffix`
  (`adapters/mqtt/transport/paho/config.go`) expands a suffix token and appends
  it to `client_id`, so one shared config file still yields a distinct id per
  replica. `hostname` appends `-<hostname>` (deterministic, human-readable in
  logs); `nonce` appends `-<8 hex>` from `crypto/rand` (unique per process). An
  unsupported token or a failed hostname lookup **fails the build** rather than
  silently colliding.

- **Nonce fails closed, never to a colliding token.** `randomClientNonce`'s
  degraded path (crypto/rand unavailable — effectively never) does **not** fall
  back to a bare timestamp, which can collide for two replicas started in the
  same tick on coarse-clock VMs. `clientNonceFallback` mixes wall-clock, PID, and
  hostname and hashes to the same width, so two replicas differing only in PID or
  host still get **distinct** tokens (finding A-15).

- **The suffix is rejected on Exclusive sessions.** `factory.NewSession` refuses
  `client_id_suffix` when `session_mode: exclusive`, because exclusive failover
  requires a stable *shared* id; the build fails rather than silently breaking
  takeover.

- **A real collision is detected, escalated, and damped, not merely retried.** A
  Session-Taken-Over disconnect feeds `noteSessionTakeover`
  (`session_lifecycle.go`): the first takeover is treated as a legitimate
  Exclusive failover and carries no penalty, but repeated takeovers without an
  intervening stable connection (`takeoverStabilityWindow`, 30s) mean two live
  instances share an id — each occurrence doubles the reconnect backoff penalty
  (`takeoverPenalty`, capped at 64s) so the mutual-eviction loop cannot spin hot,
  and an Error log names the misconfiguration. When `$share` is active on a
  non-Exclusive session — the smoking gun of a scale-out collision — the Error
  fires on the **first** occurrence. `MetricMQTTSessionTakeover` counts every
  occurrence for alerting.

## Consequences

- A replicated `$share` deployment can be made correct from one shared config by
  setting `client_id_suffix: hostname` (or `nonce`), with no per-replica config
  templating.
- A misconfiguration that would otherwise be an invisible tight reconnect loop is
  instead a bounded-backoff loop plus a named Error log plus a metric — an
  operator sees *what* is wrong, not just elevated reconnect noise.
- Exclusive failover keeps its stable shared id: the suffix cannot be enabled
  there by mistake, so the uniquifier for scale-out never sabotages single-active
  takeover.
- Uniqueness is still ultimately the operator's responsibility: nothing prevents
  deploying replicas with a hand-set identical `client_id` and no suffix. The
  bridge damps and reports that case rather than refusing to run, so a genuinely
  intentional shared id (or a broker that tolerates it) is not blocked. The
  residual risk is a documented, observable one, not a silent failure.

## Rejected alternatives

- **Mandatory suffix for any replicated deployment (enforce uniqueness at
  build).** Deferred, not adopted — the bridge cannot reliably tell "replicated
  scale-out" from "single instance" or "intentional Exclusive shared id" at build
  time, so a hard mandate would either false-positive on legitimate shared-id
  Exclusive sessions or demand deployment-topology metadata the config layer does
  not carry. The opt-in suffix plus loud, escalating collision detection covers
  the failure without a mandate that misfires on the exclusive case.
- **Silently reconnect on Session-Taken-Over with no penalty.** Rejected — that
  is the tight mutual-eviction storm itself; the streak-driven `takeoverPenalty`
  exists specifically to keep a collision from pinning the CPU and the broker.
- **Bare-timestamp nonce fallback.** Rejected — it can mint identical tokens for
  replicas started in the same tick, reintroducing the very collision the nonce
  exists to prevent; the PID/host-mixed fallback keeps the disambiguation
  property even when `crypto/rand` is unavailable.
