# Runbook: Persistent MQTT Managed-Filter Migration

**Applies to:** removal or replacement of MQTT wildcard/shared filters on a
Persistent or Exclusive session.

**Start here when:**

- dead-letter records with error code `SUBSCRIPTION_REMOVED` appear after a
  filter was removed (the runtime has a dead-letter store, `stores.dlq`); or
- the runtime has no dead-letter store, and a cutover stays below Full and
  reports that managed subscription migration requires restoring the old
  configuration.

## Why a held delivery needs settling

MQTT does not require a broker to redistribute an unacknowledged shared QoS 1/2
delivery after disconnect. A broker may pin it to the persistent session and
replay it only to the same ClientID, and it may also deliver messages it queued
for the filter while the bridge was disconnected. MQTT 5 gives a client no way
to hand such a delivery back: a PUBACK with an error reason code ends it like a
successful one. Held unacknowledged, it is resent after every reconnect and
occupies one Receive Maximum slot; enough of them stop all traffic to the
session.

- **With a dead-letter store**, GoBridge writes each such delivery to the store,
  acknowledges it after the write is durable, forgets the filter, and reaches
  Full. Nothing is lost; the records wait for you. If a write fails, the
  delivery stays unacknowledged and the session retries the cleanup with
  backoff; the process keeps running. See
  [MQTT durable session state](../transports/mqtt-durable-sessions.md#deliveries-held-for-a-removed-filter).
- **Without one**, acknowledging would lose the delivery silently. GoBridge
  preserves the exact managed-filter ledger, leaves the replay unacknowledged,
  disconnects, and enters a terminal state. It does not claim Full or portable
  redistribution. Follow [restore, drain, retry](#without-a-dead-letter-store-restore-drain-retry).

Either way, a delivery whose topic a still-desired filter also covers is not
settled as a removed filter's delivery. When a replacement overlaps the removed
filter (for example `$share/old/a/#` replaced by `$share/new/a/#`), that
delivery is live traffic for the new filter: GoBridge keeps it and delivers it
to the route once the removed filter is forgotten. It is not dead-lettered, and
without a dead-letter store it no longer fails the migration closed.

Replay settlement lasts until the current connection's replay-grace window
(`unmatched_grace`, counted from the connection coming up) ends; a delivery
does not restart the window.

## Prepare a no-buffer cutover

1. Record the old canonical broker URL, ClientID, session mode/expiry, managed
   store identity, exact filters (including the complete `$share/group/filter`),
   and handlers.
2. Stop publishers or otherwise stop new ingress matching filters being removed.
3. Let handlers and durable outbound work drain. Keep a restorable copy of the
   old configuration.
4. Apply the desired configuration. Wait for Full only after exact UNSUBSCRIBE,
   reconnect, and the `unmatched_grace` no-replay verification window complete.

Do not remove/rename an entire durable session as an ordinary live reload.
Existing sessions with no ledger baseline must first be seeded with every exact
filter or migrated under maintenance as documented in the
[MQTT transport reference](../transports/mqtt-durable-sessions.md#managed-subscription-history).

## Dead-lettered deliveries: inspect, then redrive or purge

Each record has error code `SUBSCRIPTION_REMOVED`, category `permanent`, the
session ID, the route ID of the session's single ingress route (empty when no
single route rides on the session: none or several), and `source_id`, which is
the session ID; it scopes the record to the session, so two sessions' deliveries
with the same message ID stay separate records. The dead-letter store also keeps
the removed filter as the record's address; the Admin API views do not show the
address, but they show the filter as `extra_info.subscription` on a record with
`redrive_mode` `auto`.

**Adding the filter back redrives them by itself.** When the removed filter is
added back on the same persistent or exclusive session (you roll the
configuration back, or enable the filter again) and the broker grants it,
GoBridge redrives that filter's records that are younger than
`stores.dlq.auto_redrive_window` (default `24h`) through the session's route,
and deletes each one once the route has delivered it
([automatic redrive](../http-api-admin.md#automatic-redrive)). It does not
redrive records older than the window, records with an empty `route_id`,
records filed under a route that was since renamed, or any record while the
session has no single ingress route: redrive those by hand as below. A record
whose automatic redrive failed stays in the store; its `dlq.redrive.auto` audit
record has outcome `failure` and names the error.

1. **Find the records.** The Admin API cannot filter by error code, so list the
   `permanent` entries and select on the response. Page with `offset` while
   `has_more` is `true`.

   ```bash
   curl -s -H "X-API-Key: $API_KEY" \
     "http://localhost:8080/api/v1/admin/dlq/messages?category=permanent&limit=1000" \
     | jq '.messages[] | select(.error_code == "SUBSCRIPTION_REMOVED" and .session_id == "<session-id>")'
   ```

2. **Inspect.** `GET /api/v1/admin/dlq/messages/{id}` returns one record with its
   payload. That read is audited as `dlq.read_payload`, because a payload can
   hold personal data or secrets. Decide per record whether the message must
   still be delivered.
3. **Redrive** a message that must still be delivered:
   `POST /api/v1/admin/dlq/redrive` with its IDs. Redrive re-injects it into the
   record's `route_id` through that route's **current** processors and
   bindings, and deletes the record only after the inject is confirmed. A
   record with an empty `route_id`, or whose route no longer exists, fails with
   `route or binding not found` and is kept; purge it or re-inject the payload
   by another route.
4. **Purge** a message that is no longer wanted: `POST /api/v1/admin/dlq/delete`
   with its IDs.

Warning: deleting a record destroys the only copy of the message, and it
cannot be undone. Delete by ID. Do not use `POST /api/v1/admin/dlq/purge`: it
deletes the **entire** dead-letter store, including records from every other
route.

The request and response contracts are in the
[Admin API reference](../http-api-admin.md#dlq-redrive); a walkthrough is in
[Scenario 7: DLQ with HTTP API Management](../scenarios/07-dlq-with-http-api.md#http-api-dlq-operations).

If the session keeps failing its reconcile with a transient `UNAVAILABLE`
error, the dead-letter writes are failing: the store is down, or too slow to
finish the writes of one replay-verification pass within one
`reconcile_timeout`, counted from the first write. Fix the store. The session retries by itself and the
held deliveries stay unacknowledged until a write succeeds; do not restore the
old configuration for this.

## Without a dead-letter store: restore, drain, retry

1. Keep publishers stopped. Stop the failed migration process. For Exclusive
   mode, wait at least the remaining lease TTL and verify that no replacement
   owner is active; the failed process intentionally does not release its lease.
2. Do **not** edit or empty `stores.managed_subscriptions`, change ClientID, set
   `clean_start`, expire/delete the broker session, or acknowledge the replay by
   another path. Those actions can destroy evidence or lose the delivery.
3. Start exactly one fresh runtime with the old broker/session identity, the same
   managed store, and the exact old filters and handlers.
4. Wait for the pinned delivery to be processed through its normal handler.
   Confirm source settlement and any outbox/downstream drain. Confirm the old
   broker-session backlog is empty; do not infer this merely from readiness.
5. Stop the restored runtime cleanly. Reapply the desired configuration and retry
   removal. A successful retry removes exact wildcard and shared filters,
   reconnects, completes its no-replay window, and reaches Full.
6. Resume publishers and verify a peer receives new shared traffic and
   `MQTTRouterUnmatchedDropped` did not increase for the migrated session.

If the retry fails closed again, repeat restore/drain/retry; do not bypass the
ledger. A managed-store outage or a reconciliation deadline shorter than the
verification window is also fail-closed uncertainty and must be corrected before
retry. With a dead-letter store configured, a later migration dead-letters a
pinned replay instead, and this procedure is not needed for it.
