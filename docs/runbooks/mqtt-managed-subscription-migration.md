# Runbook: Persistent MQTT Managed-Filter Migration

**Applies to:** removal or replacement of MQTT wildcard/shared filters on a
Persistent or Exclusive session.

**Start here when:** a cutover stays below Full and reports that managed
subscription migration requires restoring the old configuration.

## Why migration fails closed

MQTT does not require a broker to redistribute an unacknowledged shared QoS 1/2
delivery after disconnect. A broker may pin it to the persistent session and
replay it only to the same ClientID. GoBridge therefore preserves the exact
managed-filter ledger, leaves the replay unacknowledged, disconnects, and enters
a terminal state. It does not claim Full or portable redistribution.

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
[MQTT transport reference](../transports/mqtt.md#managed-subscription-history).

## Restore, drain, retry after a pinned replay

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
retry.
