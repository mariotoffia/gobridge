# Runbook: DynamoDB Store Outage / Throttling

**Applies to:** deployments backing the lease, outbox, or DLQ stores with
DynamoDB (the AWS store adapters).
**Audience:** on-call operators.
**Risk:** medium to high — throttling slows lease renewal and outbox drain; a
full store outage stops durable progress and can trigger failover.

## Symptom

- `LeaseAcquireLatency` / `LeaseRenewLatency` climb; `LeaseAcquireFailures`
  rise. Leadership may bounce (see [Lease flapping / split-brain](lease-flapping-split-brain.md)).
- `OutboxDepth` and `OutboxDeferred` grow while `OutboxDrainLatency` climbs —
  the drainer cannot keep up with, or cannot reach, the store.
- `DLQWriteFailures` is non-zero — DLQ records cannot be persisted.
- `OutboxDepthFailures` is non-zero — the depth query itself is erroring.
- Logs and error codes show `THROTTLED` (retryable) or `UNAVAILABLE`
  ([troubleshooting.md#throttled](../troubleshooting.md#throttled),
  [troubleshooting.md#unavailable](../troubleshooting.md#unavailable)).

## Diagnosis

1. Confirm the pressure is store-side, not broker-side. The lease and outbox
   latency metrics under `GoBridge/Runtime`
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics))
   isolate the store path:
   - `LeaseRenewLatency` rising with `LeaseAcquireFailures` — the owner is
     struggling to renew against DynamoDB, not to reach the broker.
   - `OutboxDrainLatency` rising with `OutboxDeferred` — claimed records miss
     their batch deadline because store calls are slow.
   - `OutboxDepthFailures` — the pending-count query returned a real read
     error; the drainer skipped the `OutboxDepth` gauge that cycle,
     so investigate the store, not the backlog.

2. Read the DynamoDB CloudWatch metrics for each affected table (lease, outbox,
   DLQ) and its GSIs:
   - `ThrottledRequests`, `ReadThrottleEvents`, `WriteThrottleEvents` > 0 —
     capacity is the bottleneck.
   - `ConsumedReadCapacityUnits` / `ConsumedWriteCapacityUnits` against the
     provisioned WCU/RCU (or on-demand account limits).
   - `SystemErrors` (5xx) > 0 — a service-side outage rather than throttling.

3. Separate throttling from a conditional-write conflict. Repeated
   `OutboxClaimConflicts` or
   conditional-check-failed responses on the outbox table point at claim
   contention between drainers, not raw capacity. A `STALE_FENCING_TOKEN`
   ([troubleshooting.md#stale_fencing_token](../troubleshooting.md#stale_fencing_token))
   means a lease CAS lost, expected during failover but not sustained.

4. Rule out access and shape problems that masquerade as an outage:
   - IAM: the task role must allow `dynamodb:GetItem/PutItem/UpdateItem/Query`
     and `Query` on the GSIs. A revoked grant surfaces as `NOT_AUTHORIZED`, not
     `THROTTLED`.
   - GSI: an outbox depth/claim query needs its index present and `ACTIVE`. A
     missing or backfilling GSI throttles or errors — see
     [DynamoDB outbox GSI migration](dynamodb-outbox-gsi-migration.md).

## Action

- **Throttling (`THROTTLED`, throttle events > 0):** raise provisioned WCU/RCU
  or switch the table to on-demand; if already on-demand, request a service
  quota increase. The bridge already retries `THROTTLED` with backoff, so
  restoring capacity lets the backlog drain without intervention.
- **Claim contention (`OutboxClaimConflicts` rising, low throttle):** reduce the
  number of concurrent drainers for the partition or lower the claim batch size
  rather than adding capacity — the records are being fought over, not starved.
- **Service outage (`SystemErrors`, `UNAVAILABLE`):** the bridge holds durable
  records and keeps retrying; do not purge. Wait for the AWS status to clear,
  then confirm `OutboxDepth` drains and `LeaseRenewLatency` returns to baseline.
- **IAM / GSI fault:** restore the grant or finish the index migration. Progress
  resumes on the next lease renew and drain cycle.

### Rollback criteria

If you scaled capacity or changed the table to on-demand and `OutboxDepth` is
still not draining after two poll cycles, revert the capacity change and
escalate — the bottleneck is not capacity (check `OutboxDrainStalled` for a
wedged sender, [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)).
Never delete or truncate a lease/outbox/DLQ table to clear pressure: it strands
durable records and drops in-flight messages.

## Related runbooks

- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
- [Lease flapping / split-brain](lease-flapping-split-brain.md)
- [Poison message / DLQ growth](poison-message-dlq-growth.md)
- [DynamoDB outbox GSI migration](dynamodb-outbox-gsi-migration.md)
