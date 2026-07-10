# Operational Runbooks

Symptom-first, incident-shaped runbooks: each starts from what you see, moves to
diagnosis, then to action. Every step is grounded in existing behavior — real
metric names ([monitoring](../aws-deployment/monitoring.md)), error codes
([troubleshooting](../troubleshooting.md)), and endpoints
([HTTP API](../http-api.md)).

## Incident runbooks

| Runbook | Start here when |
|---------|-----------------|
| [Broker outage / reconnect storm](broker-outage-reconnect-storm.md) | Sessions reconnect in a loop, throughput drops, `CONNECTION_LOST` / `BROKER_BUSY` in logs. |
| [Poison message / DLQ growth](poison-message-dlq-growth.md) | `DLQEntries` climbing, the same envelope fails repeatedly, `POISON_MESSAGE`. |
| [Lease flapping / split-brain](lease-flapping-split-brain.md) | Leadership bounces between instances, `LeaseTransfers` climbing, duplicate deliveries suspected. |
| [Credential expiry / rotation failure](credential-expiry-rotation-failure.md) | Auth fails after a rotation, `CredentialResolveFailure` / `CredentialStaleServed` non-zero. |
| [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md) | Lease/outbox/DLQ store slow or erroring, `THROTTLED`, `LeaseRenewLatency` / `OutboxDepth` climbing, `DLQWriteFailures`. |
| [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md) | `OutboxDepth` / `OutboxDeferred` rising, `OutboxDrainStalled` non-zero, `DrainSkippedNoLease` climbing. |
| [Node down / failover](node-down-failover.md) | An instance/task died — confirm the standby took over. `LeaseTransfers` / `LeaseExpiries` advanced. |
| [Config rollback](config-rollback.md) | A committed config change caused errors and must be reverted. |

## Procedures

| Runbook | Purpose |
|---------|---------|
| [Image upgrade / rollback and SQLite durability](upgrade-rollback-and-sqlite-durability.md) | Image version rollout and rollback; SQLite backup/restore and the durable-volume requirement. |
| [Cluster reconfiguration](cluster-reconfiguration.md) | Roll a config change across a fleet: allowed vs. disallowed live changes, drain-and-stop, convergence verification. |
| [External config writers must write atomically](external-config-atomic-writes.md) | An external tool writes the watched config file; ensure temp-file + rename, never truncate-in-place. |
| [DynamoDB outbox GSI migration](dynamodb-outbox-gsi-migration.md) | Reshape an outbox table created by an earlier build. |
