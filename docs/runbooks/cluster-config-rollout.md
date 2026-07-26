# Runbook: Cluster config rollout — whole-cohort replacement

**Applies to:** clustered deployments — `deployment_mode: clustered`, or any
instance carrying a static `cluster.endpoints` override — where multiple
instances share a config source, lease store, and outbox/DLQ stores.
**Audience:** operators applying a change that **cannot roll live**.
**Risk:** high — a mixed old/new cohort splits lease ownership and strands
durable records.

This runbook is the **manual stop-and-restart procedure**. You need it when:

- A **coordinated** cohort makes a **replacement-required** change — a durable
  session identity (client id, subscription), a lease/outbox/DLQ **store target**,
  `deployment_mode`, or the cohort's own `bridge.cluster.members` /
  `bridge.cluster.endpoints` / `bridge.cluster.rollout`. The cohort refuses to
  roll these through the barrier and names the class.
- The cohort is **not** in coordinated mode, or its config lives on a **file /
  EFS** source. These refuse every live config change, fail-closed, so every
  change is a whole-cohort replacement.

For a **live-safe** change in a **coordinated** cohort you do **not** need this
procedure — post the config and the cohort rolls it with no downtime. See
[Operating a coordinated cohort](../cluster/operating.md) for that flow and for
[which changes are live-safe](../cluster/operating.md#which-changes-roll-live-and-which-need-a-window),
and the [cluster configuration guide](../cluster/README.md) for choosing a mode.

## Why a live change is refused

Reconfiguration is **per-process**: each instance watches its own config source,
validates, and swaps its runtime independently. Without the coordinated barrier
there is **no cluster-wide version barrier, no all-member readiness gate, and no
coordinated rollback**
([Scenario 10](../scenarios/10-dynamic-reconfiguration.md#clustered-live-reload-is-rejected-fail-closed)),
so a live change would leave the cohort split across versions. The runtime
therefore refuses a live reload of a clustered deployment: the running config
keeps serving unchanged, the applied `config_version` does not advance, and the
refusal is surfaced on the failure metric (`ConfigReloads{state="failure"}`).
Discarding local durable backlog does **not** substitute for cluster consensus,
and the config file's version (optimistic-concurrency) field only guards
concurrent commits — it is **not** a cluster barrier.

A clustered config change is therefore an **externally coordinated whole-cohort
replacement**: stage, validate all, quiesce, drain/stop all, commit, start all,
verify the barrier, re-enable ingress — with whole-cohort rollback on failure.

## Procedure

Run this from the deploy/orchestration control plane (CI/CD, ECS/K8s
deployment, or scripted `deploy` job). Treat the whole cohort as one unit.

1. **Stage the new config.** Prepare the exact new config (and its target
   `version`) in a staging location the cohort does *not* yet read. Do not write
   it to the live config source yet.

2. **Validate on every member.** Confirm the staged config passes the binary's
   dry-run validation for the exact image/plugin set each member runs. A config
   that one member cannot load or validate would wedge that member — validate
   against **all** members before proceeding, not just one.

3. **Quiesce ingress.** Stop new work entering the cohort: detach the cohort from
   its load balancer / ingress (e.g. deregister the ALB target group), or pause
   the upstream producers. No new messages should arrive during the cutover.

4. **Drain and stop all members.** Drain in-flight work on every instance to its
   documented deadline, then stop **all** members. The cohort must be fully
   down — a mixed old/new cohort is exactly the split state the guard exists to
   prevent. Confirm every task/pod has reached `STOPPED`, its lease has been
   released or expired, and its outbox is drained.

5. **Commit the new config externally.** Only now write the staged config to the
   live config source (EFS file, config store, etc.), atomically
   (temp-file + rename — see
   [External config writers must write atomically](external-config-atomic-writes.md)).
   The cohort is down, so there is no live reload to race.

6. **Start all members.** Bring every member back up on the new config. A fresh
   boot into a clustered config is legitimate (it is not a live reload), so the
   guard does not fire on startup.

7. **Verify the version and readiness barrier — for every member.** Do not
   re-enable ingress until **all** members clear the barrier:
   - **Exact version:** every instance reports the **same** running
     `config_version` (the intended new version). Scrape it from
     `GET /api/v1/monitor/topology` (`config_version` field) across every
     instance; treat any divergence as a failed rollout. Cross-check the
     `running` flag to distinguish a wedged instance (no live runtime) from a
     healthy one.
   - **Full / readiness barrier:** every instance is healthy and, for
     lease-bearing exclusive sessions, has reached `ServiceLevelFull` (its
     deep-health / readiness probe passes). A member stuck below Full is not
     ready to take ingress.

8. **Re-enable ingress.** Only after every member clears step 7, re-attach the
   cohort to the load balancer / resume producers.

## Rollback (whole-cohort)

Rollback is also whole-cohort — never per-instance:

- **On timeout or failure at any step** (a member fails to validate, boot,
  converge to the target `config_version`, or reach the Full/readiness barrier
  within the deadline): keep ingress quiesced, stop **all** members, restore the
  **previous** config to the live source (the version you replaced in step 5),
  and repeat **Start all → Verify barrier → Re-enable ingress** for the old
  config.
- Do **not** leave the cohort split — do not re-enable ingress while any member
  is on a different `config_version` or below the readiness barrier.

## Recovering a stuck coordinated rollout

If a **coordinated** rollout will not resolve (deep health
`config_watch.rollout.state` stuck at `proposed` / `staging`):

- Deep health names the member the cohort is waiting on (the roster minus who has
  acked) — most often one whose own config source has not yet delivered the
  change. Confirm that member actually received the new config.
- A rollout that cannot gather every acknowledgement **aborts on its own at its
  deadline**; the running config keeps serving. Fix the config and re-post it, or
  roll the config source back to the last committed document.
- A member that restarts while the source still holds an uncommitted candidate
  boots on the last committed config — see
  [Operating a coordinated cohort](../cluster/operating.md#when-a-change-doesnt-go-through).

## Related

- [Operating a coordinated cohort](../cluster/operating.md) — the no-downtime
  flow for live-safe changes and how to read a rollout's health.
- [Cluster configuration guide](../cluster/README.md) — choosing a cluster mode.
- [Scenario 10: Dynamic Reconfiguration — Cluster Semantics and Limitations](../scenarios/10-dynamic-reconfiguration.md#cluster-semantics-and-limitations)
- [Cluster reconfiguration](cluster-reconfiguration.md) — background on
  per-process reconfiguration and cluster invariants.
- [Config rollback](config-rollback.md) — reverting a committed config change.
- [Node down / failover](node-down-failover.md) — verifying standby takeover.
