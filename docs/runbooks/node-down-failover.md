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

3. Apply the expected failover timeline for your lease preset
   (`runtime/session/config.go`):
   - **Default:** `LeaseTTL = 360s`. Worst-case takeover is bounded by roughly
     the TTL plus the new owner's acquire — expect up to ~6 minutes.
   - **HA preset (`HAConfig`):** `LeaseTTL = 45s`. Worst-case takeover is roughly
     the TTL plus acquire — expect ~45–60s.
   If more time than that has passed with no `LeaseTransfers` advance and
   `LeaseAcquireFailures` is rising, takeover is stuck — go to Action.

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

### Decision: restart vs. scale

- Restart the affected process when the runtime is terminal or the single-active
  session cannot re-establish in place.
- Scale out only if the surviving instance is healthy but saturated; adding a
  standby does not speed up a takeover already bounded by `LeaseTTL`.

## Related runbooks

- [Lease flapping / split-brain](lease-flapping-split-brain.md)
- [Scenario 8: Clustered MQTT with exclusive sessions](../scenarios/08-clustered-exclusive-sessions.md)
- [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)
