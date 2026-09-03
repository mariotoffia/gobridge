# Runbook: Poison Message / DLQ Growth

**Applies to:** any route with a DLQ store configured.
**Audience:** on-call operators.
**Risk:** medium — redriving or purging DLQ entries changes delivery state; read
the entry's `LastError` before you act.

## Symptom

- The `DLQ Growing` alarm fires: `DLQEntries` sum > 0
  ([alarms.md](../aws-deployment/alarms.md))
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
   **no** DLQ record — silent loss, alert on it directly; `DLQWriteHold` (timer,
   no dimension) is how long each DLQ write held its caller.

5. **Intake stalled during a poison burst? Read `DLQWriteHold`.** The DLQ write
   is synchronous and confirmed **before** the source delivery is settled, so
   the failure evidence is at least as durable as the message it describes. The
   cost of that guarantee is backpressure: while the DLQ store is unhealthy each
   DLQ-bound delivery holds its route goroutine — and a global in-flight slot —
   for up to the write budget. In the shipped runtime wiring that ceiling is
   **10.5 s** (2 attempts × 5 s write timeout + one 500 ms backoff); it is not
   configurable per route.

   | `DLQWriteHold` | Reading |
   |---|---|
   | p99 ≈ 0 | Healthy store; holds are noise. |
   | p99 rising, `DLQWriteFailures` flat | The store is slow but still confirming — intake throughput is already reduced. |
   | p99 at ~10.5 s with `DLQWriteFailures` rising | Every DLQ write is burning the full budget and failing. Intake for DLQ-bound traffic is effectively stopped. |
   | **No samples at all** while a route is visibly stalled | The store is ignoring cancellation — a wedge, not a slow write. The 10.5 s ceiling assumes the store honors its write deadline; the timer is only emitted when the write returns. |

   Alarm on `DLQWriteHold` p99 > 5 s for 5 minutes (half the ceiling), paired
   with `DLQWriteFailures` > 0. This is by design, not a defect: the alternative
   to holding is settling a source message whose failure evidence was never
   written. Messages are not lost during the hold — they stay unsettled and are
   redelivered.

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

  **A redrive that is not delivered keeps its entry.** An entry is deleted only
  after the route confirms the replay reached its destination. If the redriven
  message is dropped (`on_permanent_failure: drop`), filtered, expired, or
  written back to the DLQ, the redrive is reported in the `errors` array with
  the reason, counted on `DLQRedriveFailures`, and the original entry stays
  where it was. Nothing is lost by retrying too early -- the worst case is a
  207 and an unchanged DLQ.

  Two consequences to expect:

  - On a route that retains failures, a failed redrive writes a NEW entry for
    the new failure while the original stays. You will see two entries for one
    message. Delete the older one once you have confirmed they are the same
    message (the new entry's envelope carries `x-bridge.causation-id` set to the
    original envelope ID).
  - A redrive is an operator action, not a broker redelivery, so the replayed
    message is re-issued under a fresh bridge-minted envelope ID and gets the
    route's normal retry budget even when the original message came from a
    source that supplies no stable identity (the common MQTT publish).

- **Confirmed unrecoverable entries**: delete by ID (max 1000) or by filter
  (an empty filter requires `confirm_delete_all`). Purge the entire DLQ only with
  `confirm_purge_all: true`
  ([http-api.md#admin-api-endpoints](../http-api.md#admin-api-endpoints)).
- **`DLQWriteFailures` rising**: the DLQ store is unhealthy or the instance holds
  no lease. Check store health (`SQLiteStoreUnhealthy` on SQLite deployments) and
  lease ownership before assuming the messages are safe.
- **`DLQWriteHold` at the ceiling (intake stalled)**: fix the DLQ store — that is
  the only lever. Do **not** try to restore throughput by removing the DLQ store
  from the route: a route with no DLQ store drops permanently-failed messages
  with a `MessagesDropped` metric instead of recording them. Reducing the poison
  rate at the producer removes the hold at its source.
- **Hot-looping poison on AMQP 0-9-1**: `AMQP091DelayedRetryUnhonored` means the
  broker has no delayed-redelivery primitive, so a poison message requeues
  immediately. Add an `x-delivery-limit` / dead-letter-exchange guard at the
  broker ([troubleshooting.md#adapter--runtime-diagnostic-metrics](../troubleshooting.md#adapter--runtime-diagnostic-metrics)).

## Related runbooks

- [Broker outage / reconnect storm](broker-outage-reconnect-storm.md) — when the
  failures are transient connectivity, not poison payloads.
