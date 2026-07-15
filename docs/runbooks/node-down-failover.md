# Runbook: Node Down / Failover

**Applies to:** clustered deployments with exclusive (single-active) sessions
and lease coordination.
**Audience:** on-call operators.
**Risk:** medium — the mechanism is automatic, but a stuck single-active session
may need a process restart to complete takeover.

## Symptom

- An instance/task died or was replaced and you need to confirm the standby took
  over: "one node died — did failover happen?"
- `LeaseTransfers` incremented (leadership moved).
- `LeaseExpiries` incremented (the dead owner's lease lapsed).
- `LeaseAcquireFailures` — a would-be new owner cannot acquire the lease.

## Diagnosis

1. Check readiness on the surviving instance. `GET /api/v1/monitor/ready`
   accepts a `?level=` of `live`, `running`, `connected`, `subscribed`, or
   `full` (`httpapi/monitor.go`, `handleReady`). Gate on the level you need —
   `connected` proves the transport is up, `full` proves the instance reached
   its complete readiness. A 200 with `"status":"ready"` at your target level
   means this instance is serving.

2. Confirm who holds the lease. `GET /api/v1/monitor/deephealth` (authenticated)
   reports the instance `role` (`leader` once it holds a lease, otherwise
   `standby`) and a per-session `has_lease` flag
   ([http-api.md](../http-api.md)). The surviving instance should show
   `role: leader` with `has_lease: true` on the exclusive session.
   `GET /api/v1/monitor/topology` does **not** expose lease ownership — its
   `running: true` is reported by a standby too. Cross-check the lease metrics
   under `GoBridge/Runtime`
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   `LeaseTransfers` and `LeaseExpiries` should have advanced once, then settle.

3. Use the declared and measured failure-to-Full objective. A lease preset alone
   is not a failover SLO. When `routes[].session.failover_slo` is set, startup
   validates this conservative budget before stores or transports are opened:

   ```text
   lease_ttl + 2 * ceil(1.25 * acquire_poll_interval)
   + 2 * renew_call_timeout
   + complete post-takeover transport activation + startup_allowance
   <= failover_slo
   ```

   Two independent poll boundaries cover baseline establishment and the later
   threshold-crossing/takeover call. `renew_call_timeout` also bounds each whole
   Acquire call; because the manager waits the poll interval only after a call
   returns, both boundary call durations are added separately. The transport
   activation term already contains connect, cleanup/replay,
   recycle/reconnect, grace windows, and final reconciliation; do not add those
   nested phases again. The endpoint is failure detection to the successor
   reporting
   `ServiceLevelFull`. Configuration validation is necessary but not sufficient:
   compare the incident with measured warm and cold p50, p95, p99, maximum, and
   sample count from the same deployment profile. Alert on the measured
   failure-detection-to-Full interval. `LeaseTransfers` confirms ownership moved;
   it does not prove that the successor was fully serving.

   DynamoDB persists confirmed unchanged-tuple observation evidence on the lease
   row, so a replacement process can inherit elapsed confirmation without using
   cross-host wall-clock subtraction. A cold process still pays its real startup
   delay and starts observation at zero when no prior observer confirmed time.

4. Distinguish a clean takeover from a stuck single-active session. The MQTT
   (Paho) session is **single-use**: once closed it cannot be re-`Start`ed, so
   an instance re-acquiring the lease must **restart the process** to get a fresh
   session (see [Scenario 8](../scenarios/08-clustered-exclusive-sessions.md)).
   A terminal runtime fails `GET /api/v1/monitor/live` closed.

## Action

- **Takeover completed (`LeaseTransfers` advanced, standby `running`, `ready`):**
  no action. The dead node's un-acked source messages are redelivered to the new
  owner; outbox fencing prevents duplicate destination sends. Duplicates at the
  destination are still possible — downstream must stay idempotent.
- **New owner terminal / not restarting:** a single-active session that went
  terminal needs a process restart. On Kubernetes `restartPolicy: Always` (plus
  a `livenessProbe` on `/api/v1/monitor/live`) covers it; under systemd use
  `Restart=on-failure`. Readiness alone does not restart a terminal runtime — it
  only pulls the pod from the load balancer.
- **`LeaseAcquireFailures` rising, no transfer:** the lease store is unreachable
  or throttled — the standby cannot acquire. See
  [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md).
- **Leadership bouncing (`LeaseTransfers` climbing repeatedly):** this is
  flapping, not a clean failover — see
  [Lease flapping / split-brain](lease-flapping-split-brain.md).
- **`corrupt lease row` / `INVALID_CONFIG`:** stop the rollout and quiesce all
  lease users. Preserve the positive fencing version and repair the complete
  base tuple offline. Rows may omit all observation fields together, but may not
  omit `owner`, `version`, `renewed_at`, or `expires_at`; never delete/recreate a
  row because that resets fencing to version 1.

### Decision: restart vs. scale

- Restart the affected process when the runtime is terminal or the single-active
  session cannot re-establish in place.
- Scale out when no healthy standby remains or the survivor is saturated. A
  declared objective at or below 60 seconds requires a healthy continuously
  polling warm standby. The current blueprint cannot verify replica count or
  peer health; `PROD_READY_ISSUES_PLAN.md` Task 11 owns enforcement in the AWS
  deployment model.

## Related runbooks

- [Lease flapping / split-brain](lease-flapping-split-brain.md)
- [Scenario 8: Clustered MQTT with exclusive sessions](../scenarios/08-clustered-exclusive-sessions.md)
- [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)
