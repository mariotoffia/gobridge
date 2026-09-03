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
   `full`. Gate on the level you need —
   `connected` proves the transport is up, `full` proves the instance reached
   its complete readiness. A 200 with `"status":"ready"` at your target level
   means this instance is serving.

2. Confirm who holds the lease. `GET /api/v1/monitor/deephealth` (authenticated)
   reports the instance `role` -- `active` once an exclusive session holds a
   lease, `standby` while exclusive sessions are configured but none holds one,
   `standalone` when no exclusive session is configured at all -- and a
   per-session `has_lease` flag
   ([http-api-monitor.md](../http-api-monitor.md)). The surviving instance
   should show `role: active` with `has_lease: true` on the exclusive session.
   The bare `/ready` probe answers 503 for a `standby` (it is capped below
   `full`), so a standby that reads not-ready is expected, not broken.
   `GET /api/v1/monitor/topology` does **not** expose lease ownership — its
   `running: true` is reported by a standby too. Cross-check the lease metrics
   under `GoBridge/Runtime`
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   `LeaseTransfers` and `LeaseExpiries` should have advanced once, then settle.

3. Use the declared and measured failure-to-Full objective. A lease preset alone
   is not a failover SLO. When `routes[].session.failover_slo` is set, startup
   validates this conservative budget before stores or transports are opened:

   ```text
   lease_ttl + 2 * max(1ms, ceil(1.25 * acquire_poll_interval))
   + (1 + ceil(lease_ttl / min_jittered_poll)) * renew_call_timeout
   + complete post-takeover transport activation + startup_allowance
   <= failover_slo
   ```

   Two independent poll boundaries cover baseline establishment and the later
   threshold-crossing/takeover call. `renew_call_timeout` also bounds each whole Acquire call. Call latency after a
   successful observation CAS is excluded from persisted elapsed, and the
   manager waits only after each call returns. Therefore budget the baseline call
   plus every possible minimum-jitter observation round:
   `1 + ceil(lease_ttl / max(1ms, poll - (poll/2)/2))` calls. The transport
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

4. **Advisory 502/503 on exclusive routes? Read `RouteOwnerUnknown`.** The route
   locator decides who owns an exclusive route by comparing **its own** wall
   clock with the owner-written `expires_at`. That decision is advisory only —
   the locator mints no fencing token, so the data path stays skew-immune — but
   it does gate forwarding, and every unverifiable decision is counted on
   `RouteOwnerUnknown` with a `reason` dimension:

   | `reason` | Meaning | Action |
   |---|---|---|
   | `lease_expired` | This node's clock is at or past the owner's `expires_at`. | See the two causes below. |
   | `lease_unowned` | No lease row — a normal transfer window. | None if it settles within one acquire poll. |
   | `store_unavailable` | The lease store failed and no usable cached owner remains. | [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md). |
   | `store_breaker_open` | The locator refused without calling a repeatedly-failing store. | As above; the breaker closes on the first success. |

   A sustained `lease_expired` count has exactly two causes:

   - **Fleet clock skew.** The owner renewed against its clock and considers the
     lease live; this node's clock disagrees by more than the renew margin.
     Budget fleet skew below one renew interval (default clustered profile: 45 s
     `lease_ttl`, so keep skew under a few seconds) and verify NTP/chrony on
     every node before suspecting the lease store. Skew never corrupts data —
     it degrades routing availability.
   - **Cold takeover after a whole-fleet restart.** Takeover requires a full
     local lease-observation window regardless of how long ago the row expired,
     so an hours-expired row from a previous generation still costs about one
     `lease_ttl` before the first acquisition. Expect `lease_expired` for that
     window after a full-fleet stop/start and include it in recovery objectives:
     it is additive to, not covered by, the `failover_slo` above, which measures
     failure detection on a **running** cohort.

   Alarm on `RouteOwnerUnknown` (`reason=lease_expired`) sustained beyond one
   `lease_ttl` plus one acquire poll — past that it is skew or a stuck acquire,
   not a takeover in progress.

5. Distinguish a clean takeover from a stuck single-active session. The MQTT
   (Paho) session is **single-use**: once closed it cannot be restarted, so
   an instance re-acquiring the lease must **restart the process** to get a fresh
   session (see [Scenario 8](../scenarios/08-clustered-exclusive-sessions.md)).
   A terminal runtime fails `GET /api/v1/monitor/live` closed.

6. A **session failure** is not a node failure and must not cost a TTL. When a
   reconnect reconcile fails, or the transport's event stream dies, the owner
   closes its source, releases the lease, and the supervisor restarts that one
   session. On the default `connect_after_lease` profile the restarted session
   briefly re-seizes the lease (the store's same-owner path grants it at once)
   and only then discovers the single-use transport refuses to start again. That
   node is now provably dead, so it **releases** before going terminal: a standby
   takes over within one `acquire_poll_interval`, not one `lease_ttl`.

   So a `LeaseTransfers` advance that lags a session failure by roughly a full
   `lease_ttl` is a symptom, not the design — check that the restarting node
   logged `lease released` with `reason=deferred connect failure` before it went
   terminal.

## Action

- **Takeover completed (`LeaseTransfers` advanced, standby `running`, `ready`):**
  no action. The dead node's un-acked source messages are redelivered to the new
  owner. Outbox fencing prevents a stale owner from committing or continuing
  outbox work on a fenced record — it does **not** undo a destination send the
  old owner already had accepted before it lost the response or died. Duplicates
  at the destination are therefore still possible; downstream must stay
  idempotent.
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
  polling warm standby. In the shipped AWS deployment model the
  [`GoBridgeDynamoDBHA` CDK construct](../../deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha)
  now enforces this — at least two workers (`WorkerDesiredCount` may never be
  below two) spread across a required two AZs — so any single task loss leaves a
  continuously polling standby. Only a standalone (non-CDK) deployment still
  cannot verify replica count or peer health; there the operator must enforce
  and verify the invariant in their own orchestrator.

## Related runbooks

- [Lease flapping / split-brain](lease-flapping-split-brain.md)
- [Scenario 8: Clustered MQTT with exclusive sessions](../scenarios/08-clustered-exclusive-sessions.md)
- [DynamoDB store outage / throttling](dynamodb-store-outage-throttling.md)
