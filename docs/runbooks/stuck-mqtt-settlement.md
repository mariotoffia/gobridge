# Runbook: Stuck MQTT settlement

**Applies to:** MQTT ingress on any session mode. The recovery differs by mode.
**Audience:** on-call operators.
**Risk:** medium — the bridge recovers a stalled window on its own, but every
recovery redelivers messages, and the wrong intervention converts a stall into
duplicates or loss.

## Symptom

An MQTT session is connected, readiness is green or nearly so, no route is
erroring, and yet throughput has fallen or stopped. The signals are gauges, not
errors:

- `MQTTOldestUnsettledAge` climbing and not falling back.
- `MQTTReceiveWindowUtilization` approaching `1.0`.
- `MQTTUnsettled` sitting at or near the session's `receive_maximum`.
- `MQTTSessionRecoveryRecycle` advancing — the session is recycling itself.
- `MQTTReceiverEmitRejected` non-zero, usually `outcome=recovering` first.

There is no error code for this. A stalled settlement is a *slow* path, not a
failing one, so `troubleshooting.md` will not name it and the route reports no
failure.

## What is actually happening

The adapter tracks every current-connection QoS 1/2 packet from receipt until a
successful protocol Ack. A packet is settled only after the route accepts it —
outbox persist for `shared_outbox`, target accept for `direct_hold`. Until then
it holds one of the broker's Receive-Maximum slots. When settlement slows, the
slots fill; when they are all held, the broker stops sending and ingress halts.

`MQTTUnsettled` is that held count and `MQTTReceiveWindowUtilization` is
`unsettled / receive_maximum`. **The window is the symptom, not the cause.** The
cause is downstream of MQTT in every case below except a wedged route runner.

## Diagnosis

Read the per-session state before acting
([HTTP API](../http-api.md)):

```bash
curl -s -H "X-API-Key: ${ADMIN_KEY}" \
  "http://<host>:8080/api/v1/monitor/deephealth" \
  | jq '.sessions[] | {session_id, unsettled_count, oldest_unsettled_age_ms,
                       receive_window_utilization, recovery_recycle_count, service_level}'
```

Each session reports `unsettled_count`, `oldest_unsettled_age_ms`,
`receive_window_utilization`, and `recovery_recycle_count` — the same four values
the gauges publish, without waiting for a metrics flush.

1. **`oldest_unsettled_age_ms` large, `unsettled_count` at the window, routes
   `ready`.** The downstream is slow, not broken. For `shared_outbox` the store
   is the suspect — cross-check `OutboxPersistLatency` and the
   [DynamoDB store outage runbook](dynamodb-store-outage-throttling.md). For
   `direct_hold` the target transport is the suspect: settlement waits for the
   destination broker's PUBACK/PUBCOMP.

2. **`unsettled_count` at the window with `oldest_unsettled_age_ms` small and
   churning.** Not a stall — the session is running at its ceiling. Sustained
   utilization near `1.0` means `receive_maximum`, not the network, is the
   throughput limit: `max sustained msg/s ≈ receive_maximum / settlement latency`
   ([MQTT behaviour](../transports/mqtt-behavior.md)). Raise `receive_maximum`
   or route `max_in_flight` (both are counts; see
   [ingress limits are counts](../transports/mqtt-options.md#ingress-limits-are-counts)),
   or reduce settlement latency.

3. **`MQTTReceiverEmitRejected` non-zero.** The route pipeline refused a delivery
   at emit — a shutting-down or wedged route runner, not a slow one. Read the
   `outcome` tag; it decides whether anything was lost:
   - `outcome=recovering` — QoS 1/2 on a Persistent or Exclusive session. Left
     un-acked; a bounded session recycle makes the broker redeliver it. Nothing
     is lost. This count is the leading indicator of the recycles below.
   - `outcome=lost` — QoS 0, or QoS 1/2 on an **Ephemeral** session where
     `clean_start=true` leaves no session to resume. Acked and dropped so it
     stops pinning a window slot. **This is acknowledged loss.** Cross-check
     `RouteRestarts` and `route_dead` in deep health for the wedged route.

4. **`MQTTSessionRecoveryRecycle` advancing.** The adapter is recovering itself:
   it disconnects and reconnects with `clean_start=false`, and the broker
   redelivers every unsettled packet. Each recycle therefore duplicates *innocent*
   in-flight messages too — absorbed by `shared_outbox` identity dedup, and
   **unmeasured on `direct_hold`**. Completed attempts are spaced at least 30
   seconds apart, so a steady advance means the downstream keeps failing hard,
   not that recovery is broken. See
   [MQTT settlement recovery](../transports/mqtt-settlement-recovery.md).

5. **Readiness below Full while recycling.** Expected: readiness drops
   synchronously when recovery is queued and only returns once the exact-epoch
   replacement reconcile succeeds. A readiness that never returns means recovery
   is failing — check for `MQTTSessionResumeLost` (the broker had no session to
   resume, so recovery cannot complete) and for a terminal session error.
   `SessionRebuilds` advancing for the session means the bridge is rebuilding it
   after such an error (see the action for case 5).

6. **`MQTTAckAfterReconnect` non-zero.** Settlements whose protocol ack could not
   reach the broker. Each count is a guaranteed redelivery. It explains
   duplicates *after* the stall clears; it is not itself a cause of the stall.

## Action

- **Slow downstream (case 1).** Fix the downstream. The window recovers on its
  own as settlement drains; no MQTT-side action is needed or safe. Do **not**
  raise `receive_maximum` to mask it — a wider window over the same slow
  downstream holds more messages un-acked and lengthens every recycle.
- **Ceiling reached (case 2).** Raise `receive_maximum` and/or route
  `max_in_flight`. GoBridge does not estimate memory, so watch the process
  memory under load after the change; a wider window holds more messages at
  once.
- **Wedged route (case 3).** Restart the process that owns the session. QoS 1/2
  on a Persistent or Exclusive session is redelivered on resume; QoS 0 and
  Ephemeral QoS 1/2 in flight are lost, which is the loss the `outcome=lost`
  count already recorded. Then fix the route: a route that keeps failing to start
  shows up on `RouteRestarts` at the backoff cap.
- **Repeated recycles (case 4).** Treat the downstream failure as the incident.
  Every recycle is a duplicate burst; on a `direct_hold` route with no downstream
  dedup, that burst reaches the destination. If the route cannot be made
  idempotent, move it to `shared_outbox` so the outbox identity absorbs
  redelivery.
- **Recovery not completing (case 5).** A session whose recovery fails latches
  a permanent error, and that session instance never starts again. Under the
  Supervisor (`cmd/gobridge`) or the AWS runtime the bridge rebuilds the
  session's reload unit in place after a backoff (1 s, doubling to 30 s): the
  unit's routes stop, the failed session closes, and a fresh session connects.
  Every other unit keeps running, so do not replace the task
  ([ADR 0020](../adr/0020-contain-unrecoverable-session-by-unit-rebuild.md)).
  Watch:
  - `SessionRebuilds` (tagged `session_id`): one count per rebuild;
  - deep health: the session reads `ready: false` with `service_level: none`
    until the rebuild replaces it, and readiness counts it as not ready.

  The process still restarts — replace the task if nothing restarts it — when
  the runtime has no rebuild handler, when the failed session's close did not
  complete, or when the rebuild fails or leaves the session failed. A
  `SessionRebuilds` rate that keeps climbing means every fresh session fails the
  same way. Verify `session_expiry_interval` exceeds the outage window, or the
  broker will keep answering `Session Present=false` and recovery will keep
  failing.

### What NOT to do

- **Do not set `clean_start: true` to clear a stuck window.** It discards the
  broker-side session, and with it every queued offline QoS 1/2 message for that
  `client_id` — silent, unrecoverable loss, on every restart thereafter.
- **Do not lower `session_expiry_interval` to force a fresh session.** Same loss,
  delayed.
- **Do not restart repeatedly to "unstick" it.** If the downstream is slow, each
  restart redelivers the whole unsettled window and makes the backlog worse.
- **Do not treat `MQTTUnsettled` at the window as an MQTT fault.** It is
  backpressure working as designed: the bridge is refusing to accept more than it
  can settle, which is what keeps QoS 1/2 at-least-once.

## Related runbooks

- [Broker outage / reconnect storm](broker-outage-reconnect-storm.md)
- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
- [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)
- [MQTT SUBACK rejection flap / QoS downgrade](mqtt-suback-rejection-flap.md)
- [MQTT ingress poison](mqtt-ingress-poison.md)
