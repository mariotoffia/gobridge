# Runbook: Outbox Backlog / Stuck Drain

**Applies to:** routes using `delivery_mode: shared_outbox` (durable outbox).
**Audience:** on-call operators.
**Risk:** medium — a stuck drain grows a durable backlog and delays delivery;
a wrong purge loses messages.

## Symptom

- `OutboxDepth` climbs and does not fall back toward zero.
- `OutboxDeferred` rises — claimed records miss their batch deadline.
- `OutboxDrainStalled` is non-zero — a sender is wedged.
- `DrainSkippedNoLease` climbs on a route that is supposed to drain.
- `OutboxStranded` is non-zero after an explicitly forced destructive reload.

## Diagnosis

Each metric names a distinct cause; read them before acting
([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)).

1. `OutboxDepth` is the true PENDING backlog gauge (`domain/shared/metrics.go:25-50`).
   A steadily rising value with normal drain latency means ingress outpaces the
   drainer — scale drain throughput. On a store without `OutboxDepthReporter`
   the depth falls back to the claimed count and saturates at the batch size, so
   confirm against `OutboxClaimBatchSize` (a liveness signal, not backlog).

2. `OutboxDrainStalled` is the single signal that a **sender is wedged** —
   in-flight sends did not return within a grace past the batch deadline,
   the signature of a `Sender` that ignores context cancellation
   (`domain/shared/metrics.go:97-105`). The runtime does not kill the wedged
   goroutines; this is a diagnostic, not a self-recovery, signal. If it is
   non-zero, the partition is stuck on the target transport, not the store.

3. `DrainSkippedNoLease` counts drain cycles skipped because the drainer holds
   no lease (`domain/shared/metrics.go:91-96`). A short burst on a standby is
   normal. A continuously-rising value on a route that should drain means a
   **misconfigured lease** — commonly a `shared_outbox` route bound to a
   non-exclusive session that never acquires a lease.

4. `OutboxDeferred` rising under load flags a drain budget too small for the
   batch size (`domain/shared/metrics.go:77-82`) — records are claimed but the
   batch deadline expires before the sends complete.

5. `OutboxStranded` (non-zero) means a reload explicitly authorized with
   `WithAllowDestructiveReload` left observable pending records with no drainer
   (`domain/shared/metrics.go:106-118`). Non-forced orphaning reloads are refused
   before swap. Cross-check [Cluster reconfiguration](cluster-reconfiguration.md).

6. Confirm the drainer is actually running: `GET /api/v1/monitor/topology`
   (authenticated) shows `running` and the route list
   ([http-api.md](../http-api.md)). If the target transport is down, expect
   `CONNECTION_LOST` / `UNAVAILABLE` in logs ([troubleshooting.md](../troubleshooting.md)).

## Action

- **Sender wedged (`OutboxDrainStalled` non-zero):** the sending goroutine will
  not clear on its own. Restart the process that holds the drain to free the
  batch, then fix the sender/transport (an unreachable endpoint, a broker that
  never returns). The durable records survive the restart and are re-claimed.
- **No lease (`DrainSkippedNoLease` rising):** fix the config — bind the
  `shared_outbox` route to an exclusive session that acquires a lease. Until an
  instance holds the lease, nothing drains by design.
- **Ingress outpaces drain (`OutboxDepth` rising, latency normal):** raise drain
  throughput (scale the active instance, increase the claim batch size) and
  confirm the store is not throttling ([DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)).
- **Budget too small (`OutboxDeferred` rising):** increase the batch deadline or
  reduce the batch size so claimed records finish within the cycle.
- **Stranded records (`OutboxStranded` non-zero):** restore a route/session that
  drains the orphaned partition, or drain it manually; do not leave it.

### When NOT to purge

Do **not** purge or truncate the outbox to clear a backlog. Pending records are
messages the source already acknowledged after outbox persistence
(`ack_after: outbox_persist`); deleting them is silent, unrecoverable loss.
Because a manual purge bypasses the runtime, it emits **no** `MessagesDropped`
(or any other) metric to record what was lost — the deletion is invisible to
observability, which is exactly what makes it dangerous. Purge is only
defensible for records that are provably undeliverable AND already accounted for
downstream — never as a way to make a depth alarm go quiet.

## Related runbooks

- [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)
- [Lease flapping / split-brain](lease-flapping-split-brain.md)
- [Cluster reconfiguration](cluster-reconfiguration.md)
- [Node down / failover](node-down-failover.md)
