# Runbook: Cluster Reconfiguration

**Applies to:** clustered deployments (multiple instances sharing a config
source, lease store, and outbox/DLQ stores).
**Audience:** operators running a config change across a fleet.
**Risk:** high — some live changes strand durable records or split lease
ownership if rolled instead of drained.

## Overview

Reconfiguration is **per-process**: each instance watches its own config source,
reloads, validates, and swaps its runtime independently. There is **no
cluster-wide version barrier and no coordinated rollback**
([Scenario 10](../scenarios/10-dynamic-reconfiguration.md#cluster-semantics-and-limitations)).
During a rollout the fleet runs a mix of old and new definitions until every
instance has converged — indefinitely if one stays wedged on a config it cannot
load. This runbook covers which changes are safe to roll live, which must be
drained and stopped, and how to verify convergence.

## Allowed vs. disallowed live changes

**Safe to roll live** (eventually consistent across the fleet): routing, policy,
and transformation changes. The same message class may be handled under the old
or new definition during the divergence window, but no records are stranded and
no lease is split.

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
Supervisor rejects the reload and keeps the OLD runtime serving (metric
`ConfigReloads{state="failure"}`):

- `destructiveReloadShape` gate on a paused/resume reload (`bridge/supervisor.go:620`).
- `storeIdentityChanged` — store `type`/path/table change (`bridge/supervisor.go:690`).
- `leaseSessionIDChanged` — lease-bearing exclusive `session_id` change
  (`bridge/supervisor.go:701`).

The only override is `WithAllowDestructiveReload` (`bridge/supervisor.go:267-277`),
which discards the stranded backlog by design — do not set it for a routine
change. Separately, `config/validate.go` rejects clustered-invalid shapes (for
example cluster endpoint and clustered exclusive HTTP `direct_hold` rules,
`config/validate.go:62-63`) at load, before any swap.

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
handlers, drain, then retry. `WithAllowDestructiveReload` does not make this
safe, and GoBridge does not claim portable broker redistribution.

## Rollback

- A failed swap keeps the OLD runtime running on each instance — the per-process
  refusal is itself the local rollback for an invariant change.
- To revert a committed config, re-commit the previous config through the admin
  transaction flow, or restore the config file at its source
  ([Config rollback](config-rollback.md),
  [HTTP API — Config transactions](../http-api.md#config-transactions)). The
  `version` CAS field guards concurrent commits to a shared file; it does not
  gate the per-instance apply, so it is not a cluster rollout barrier.

## Verify convergence

Scrape the running config version from every instance and confirm they agree:

```bash
for host in $FLEET; do
  curl -s -H "X-API-Key: $MONITOR_API_KEY" \
    "https://$host/api/v1/monitor/topology" | jq '{host: "'"$host"'", config_version, running}'
done
```

`GET /api/v1/monitor/topology` exposes `config_version` when a config provider is
wired (`httpapi/monitor.go:186-211`) alongside `running`. Every instance should
report the same `config_version` with `running: true`. Treat persistent version
divergence as an alertable condition; if an instance lags, it is wedged on a
config it cannot load — inspect its logs before forcing anything.

## Related runbooks

- [Config rollback](config-rollback.md)
- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
- [Node down / failover](node-down-failover.md)
- [Persistent MQTT managed-filter migration](mqtt-managed-subscription-migration.md)
- [Scenario 10: Dynamic reconfiguration](../scenarios/10-dynamic-reconfiguration.md)
