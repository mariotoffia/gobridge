# Runbook: Poison Message / DLQ Growth

**Applies to:** any route with a DLQ store configured.
**Audience:** on-call operators.
**Risk:** medium — redriving or purging DLQ entries changes delivery state; read
the entry's `LastError` before you act.

## Symptom

- The `DLQ Growing` alarm fires: `DLQEntries` sum > 0
  ([monitoring.md#cloudwatch-alarms](../aws-deployment/monitoring.md#cloudwatch-alarms))
  or the guide's `DLQEntries` sum > 100 alert
  ([deployment-guide.md#observability](../deployment-guide.md#observability)).
- Logs show `POISON_MESSAGE`, `INVALID_PAYLOAD`, or `SCHEMA_VIOLATION`.
- The same envelope reappears and fails repeatedly before landing in the DLQ.

## Diagnosis

1. Read the DLQ summary and page the entries
   ([http-api.md#admin-api-endpoints](../http-api.md#admin-api-endpoints)):

   ```bash
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/admin/dlq"                       # {configured,count,count_capped}
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/admin/dlq/messages?route_id=<id>&limit=20"
   ```

2. Open a single entry to get the real cause. The `LastError` field carries the
   underlying failure — treat that as the thing to fix
   ([troubleshooting.md#poison_message](../troubleshooting.md#poison_message)):

   ```bash
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/admin/dlq/messages/<id>"         # audited as dlq.read_payload
   ```

3. Classify from the error code
   ([troubleshooting.md](../troubleshooting.md)): `POISON_MESSAGE` means retries
   and replay attempts were exhausted; `INVALID_PAYLOAD` / `SCHEMA_VIOLATION`
   mean the message will never parse and must be fixed at the producer.

4. Separate the counters ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   `DLQEntries` (`route_id`, `category`) is the DLQ write count; `DLQWriteFailures`
   (no dimension) means the DLQ store itself is rejecting writes or no lease was
   held; `MessagesDropped` (`route_id`, `reason`) is a terminal drop that wrote
   **no** DLQ record — silent loss, alert on it directly.

## Action

- **Producer-side defect** (`INVALID_PAYLOAD` / `SCHEMA_VIOLATION`): fix the
  producer, then decide per entry whether to redrive or delete. Redriving an
  unparseable message just re-fills the DLQ.
- **Transient downstream cause now resolved**: redrive by ID (max 100 per call,
  207 on partial failure). Watch `DLQRedrives` / `DLQRedriveFailures`:

  ```bash
  curl -s -X POST -H "X-API-Key: ${ADMIN_KEY}" -H "Content-Type: application/json" \
    -d '{"ids":["<id1>","<id2>"]}' \
    "http://<host>:8080/api/v1/admin/dlq/redrive"
  ```

- **Confirmed unrecoverable entries**: delete by ID (max 1000) or by filter
  (an empty filter requires `confirm_delete_all`). Purge the entire DLQ only with
  `confirm_purge_all: true`
  ([http-api.md#admin-api-endpoints](../http-api.md#admin-api-endpoints)).
- **`DLQWriteFailures` rising**: the DLQ store is unhealthy or the instance holds
  no lease. Check store health (`SQLiteStoreUnhealthy` on SQLite deployments) and
  lease ownership before assuming the messages are safe.
- **Hot-looping poison on AMQP 0-9-1**: `AMQP091DelayedRetryUnhonored` means the
  broker has no delayed-redelivery primitive, so a poison message requeues
  immediately. Add an `x-delivery-limit` / dead-letter-exchange guard at the
  broker ([troubleshooting.md#adapter--runtime-diagnostic-metrics](../troubleshooting.md#adapter--runtime-diagnostic-metrics)).

## Related runbooks

- [Broker outage / reconnect storm](broker-outage-reconnect-storm.md) — when the
  failures are transient connectivity, not poison payloads.
