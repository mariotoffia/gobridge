# Runbook: Cluster Reconfiguration

**Applies to:** clustered deployments (multiple instances sharing a config
source, lease store, and outbox/DLQ stores).
**Audience:** operators running a config change across a fleet.
**Risk:** high — some live changes strand durable records or split lease
ownership if rolled instead of drained.

## Overview

**Current behaviour: a clustered live reload is rejected
fail-closed.** Every non-no-op live reload of (or into) a clustered deployment is
refused by the runtime and by the AWS composition root, keeping the running
instance on its last-good config. Clustered config changes MUST go through the
externally coordinated whole-cohort procedure in
[Cluster Config Rollout](cluster-config-rollout.md) (stage → validate all →
quiesce → drain/stop all → commit → start all → verify version/readiness barrier
→ re-enable ingress, with whole-cohort rollback). A genuine no-op re-emit is
still accepted.

**Historical hazard this guard now prevents.** Reconfiguration is **per-process**:
each instance watches its own config source, reloads, validates, and swaps its
runtime independently, with **no cluster-wide version barrier and no coordinated
rollback**
([Scenario 10](../scenarios/10-dynamic-reconfiguration.md#cluster-semantics-and-limitations)).
Before, rolling a config change through a cohort therefore let the fleet run a
**mix of old and new definitions** until every instance converged — indefinitely
if one stayed wedged on a config it could not load. That split-version window is
exactly what the fail-closed guard now blocks: local config CAS / reference
tracking is **not** cluster consensus and never was a version barrier, so it must
not be relied on for live cohort reconfiguration. The classification below
describes single-process (standalone) reload behaviour and the *reasons* each
change is unsafe to roll across a cohort.

## Allowed vs. disallowed live changes

**Safe to roll live in a standalone (single-process) deployment**
(eventually consistent across a fleet if you were ever to run one, but in a
**clustered** deployment even these are refused live — replace the cohort per the
runbook above): routing, policy, and transformation changes. The same message
class may be handled under the old or new definition during a divergence window,
but no records are stranded and no lease is split.

**Disallowed live / rolling** — these are **cluster invariants**. Changing any of
them under a rolling reload splits ownership or strands durable records:

- A route or session `session_id` on a lease-bearing exclusive session — changes
  the **lease identity**, so two instances can hold "its" lease in different keys
  and drain independently (duplicate sends + stranded backlog).
- A lease, outbox, DLQ, or managed-subscription store's `type` or backing
  path/table — repointing a store live strands durable records/history.
- Removing an outbox/DLQ store, orphaning a `shared_outbox` partition, or
  removing/renaming a persistent/exclusive MQTT session identity.

These are **hard-refused at swap time**, per process, not merely warned. The
bridge rejects the reload and keeps the OLD runtime serving (metric
`ConfigReloads{state="failure"}`):

- A destructive reload shape on a paused/resume reload.
- A store `type`/path/table change.
- A lease-bearing exclusive `session_id` change.

The only override discards the stranded backlog by design — do not set it for a routine
change. Separately, config validation rejects clustered-invalid shapes (for
example cluster endpoint and clustered exclusive HTTP `direct_hold` rules) at load, before any swap.

## Action — drain-and-stop for an invariant change

Roll a cluster-invariant change with a full stop/restart, never a rolling reload:

1. Stop new ingress or accept the drain window.
2. Wait for `OutboxDepth` to reach zero on every instance
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)) —
   confirm no `OutboxStranded` and no pending records before proceeding
   ([Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)).
3. Stop **every** instance.
4. Deploy the new config to the shared source.
5. Restart the fleet.

Persistent/exclusive MQTT filter removal has a stricter protocol because a
broker may pin an unacknowledged shared delivery to the old ClientID. Preserve
the managed-filter ledger and follow the
[persistent MQTT managed-filter migration runbook](mqtt-managed-subscription-migration.md);
a terminal migration-required result means restore the exact old identity and
handlers, drain, then retry. The destructive-reload override does not make this
safe, and GoBridge does not claim portable broker redistribution.

## Rollback

- A refused reload keeps the OLD runtime running on each instance — for a
  clustered deployment every non-no-op live reload is refused fail-closed, so the
  per-process refusal is itself the local rollback (no member ever adopts the
  split-inducing config).
- **To revert a committed config across a clustered cohort, roll back the whole
  cohort externally — do _not_ re-commit or live-restore the previous config.** A
  re-commit through the admin transaction flow bumps the config version, and
  any content difference is a non-no-op, so a per-process live reload of the
  previous config is **refused fail-closed** just like the forward change (finding
  Follow
  [Cluster Config Rollout — Rollback (whole-cohort)](cluster-config-rollout.md#rollback-whole-cohort):
  keep ingress quiesced, stop **all** members, restore the previous config to the
  live source, then restart all members behind the version/readiness barrier. The
  `version` CAS field guards concurrent commits to a shared file; it does not gate
  the per-instance apply and is **not** a cluster rollout or rollback barrier.
- Only in a **standalone** (single-process) deployment may you revert by
  re-committing the previous config through the admin transaction flow or
  restoring the config file at its source
  ([Config rollback](config-rollback.md),
  [HTTP API — Config transactions](../http-api.md#config-transactions)).

## Verify convergence

Scrape the running config version from every instance and confirm they agree:

```bash
for host in $FLEET; do
  curl -s -H "X-API-Key: $MONITOR_API_KEY" \
    "https://$host/api/v1/monitor/topology" | jq '{host: "'"$host"'", config_version, running}'
done
```

`GET /api/v1/monitor/topology` exposes `config_version` when a config provider is
wired alongside `running`. Every instance should
report the same `config_version` with `running: true`. Treat persistent version
divergence as an alertable condition; if an instance lags, it is wedged on a
config it cannot load — inspect its logs before forcing anything.

## Related runbooks

- [Config rollback](config-rollback.md)
- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
- [Node down / failover](node-down-failover.md)
- [Persistent MQTT managed-filter migration](mqtt-managed-subscription-migration.md)
- [Scenario 10: Dynamic reconfiguration](../scenarios/10-dynamic-reconfiguration.md)
