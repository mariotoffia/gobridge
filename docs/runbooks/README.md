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
| [Config rollback](config-rollback.md) | A committed config change caused errors and must be reverted. |

## Procedures

| Runbook | Purpose |
|---------|---------|
| [Image upgrade / rollback and SQLite durability](upgrade-rollback-and-sqlite-durability.md) | Image version rollout and rollback; SQLite backup/restore and the durable-volume requirement. |
| [DynamoDB outbox GSI migration](dynamodb-outbox-gsi-migration.md) | Reshape an outbox table created by an earlier build. |
