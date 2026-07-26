# Runbook: Orchestrator Kill Before Shutdown Drain

**Applies to:** any deployment; both binaries.
**Audience:** on-call operators.
**Risk:** medium — the orchestrator `SIGKILL`ed (or forced) the process before
its graceful shutdown budget finished. Durable, at-least-once traffic survives
and is redelivered; best-effort traffic in flight at the kill is lost.

## Background

On `SIGTERM`/`SIGINT` GoBridge drains in phases: settle in-flight work (bounded
by `drain_timeout`), release leases, close transports and stores, then stop
HTTP — the whole sequence bounded by `shutdown_timeout`
([deployment-guide.md](../deployment-guide.md#health-checks-and-graceful-shutdown)).
The orchestrator must give the container **more** stop time than
`shutdown_timeout`, or it sends `SIGKILL` mid-drain. A second `SIGINT`/`SIGTERM`
also forces an immediate exit before drain completes.

## Symptom

Evidence the process was killed/forced **before** graceful drain finished:

- No clean `exit 0`: the orchestrator reports `SIGKILL` (exit `137` =
  `128+9`) or exit code `2` (a second signal forcing immediate exit —
  [exit codes](../deployment-guide.md#exit-codes)).
- The shutdown log stops partway — no final "clean shutdown" line, phases
  (drain → lease release → store close) truncated.
- The orchestrator's stop/termination grace is at or below `shutdown_timeout`
  (a config bug, not an incident-time action).

## Blast radius

- **QoS 1/2 (at-least-once) and durable `shared_outbox` records: preserved.**
  Unsettled deliveries were never acked to the source, so the broker
  redelivers on restart; persisted outbox records are recovered by the drainer.
  Expect **duplicates** on routes without downstream dedup.
- **QoS 0 and pre-`Persist` Ephemeral traffic: can be lost.** Anything accepted
  but not yet settled/persisted at the kill carries no delivery contract and is
  gone — it is not redelivered.

## Diagnosis / what to check after restart

1. **Outbox depth** — `OutboxDepth` should show the recovered backlog draining
   after restart; a persistent non-draining depth is a separate incident
   ([outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)).
2. **QoS 0 drop counters** — `MQTTRouterStalePurged` / `MQTTRouterDropped`
   around the kill window quantify best-effort loss (QoS 0 only; QoS 1/2 counted
   in `MQTTRouterStalePurged` is redelivered, not lost).
3. **`MQTTAckAfterReconnect`** after restart — a burst predicts broker
   redelivery duplicates; verify downstream idempotency on `direct_hold` routes.

## Remediation

- **Fix the grace window** so it does not recur: set the orchestrator's
  stop/termination timeout **higher than** `shutdown_timeout` (ECS
  `stopTimeout`, Kubernetes `terminationGracePeriodSeconds`, systemd
  `TimeoutStopSec`), and keep `drain_timeout` shorter than `shutdown_timeout`
  for headroom.
- **No config rollback is needed** — this is a lifecycle-timing failure, not a
  bad config. Let recovered records drain and confirm the counters above settle.

## Related

- [Image upgrade / rollback and SQLite durability](upgrade-rollback-and-sqlite-durability.md)
- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
