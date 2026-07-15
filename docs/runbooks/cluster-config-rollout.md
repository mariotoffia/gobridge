# Runbook: Cluster Config Rollout (whole-cohort replacement)

**Applies to:** clustered deployments — `deployment_mode: clustered`, or any
instance carrying a static `cluster.endpoints` override — where multiple
instances share a config source, lease store, and outbox/DLQ stores.
**Audience:** operators changing the config of a running cohort.
**Risk:** high — an uncoordinated rollout splits the cohort across config
versions, splitting lease ownership and stranding durable records.

## Why there is no live rollout

Reconfiguration is **per-process**. Each instance watches its own config source,
validates, and swaps its runtime independently. There is **no cluster-wide
version barrier, no all-member readiness gate, and no coordinated rollback**
([Scenario 10](../scenarios/10-dynamic-reconfiguration.md#clustered-live-reload-is-rejected-fail-closed)).

Because of that, the runtime **refuses every non-no-op live reload of (or into) a
clustered deployment, fail-closed** (finding H8). The guard runs in both reload
paths — `bridge.Supervisor.apply` and the AWS file-based composition root
`bootstrap.App.applyLogicalConfig` — after no-op detection but before any
build or stop. On refusal the current runtime keeps serving unchanged, the
applied `config_version` does not advance, and the failure is surfaced through
the existing path (`ConfigReloads{state="failure"}` counter and the failed
`SwapEvent`; on the AWS root, a returned error that keeps the last-good runtime
and reports `committed_not_applied` for an admin commit).

`WithAllowDestructiveReload` does **not** bypass this guard: it only discards
*local* durable backlog and cannot substitute for cluster consensus.

> **Local CAS / reference tracking is not cluster consensus.** The
> `BridgeConfig.Version` optimistic-concurrency (CAS) field and the per-process
> applied-config reference tracking only guard concurrent *commits* to a shared
> config file and make per-process reloads idempotent. They are **not** a
> cluster version barrier, **not** distributed consensus, and provide **no**
> all-member readiness gate or coordinated rollback. Do **not** describe or rely
> on them as resilient live reconfiguration across a cohort.

A clustered config change is therefore an **externally coordinated whole-cohort
replacement**: stage, validate all, quiesce, drain/stop all, commit, start all,
verify the barrier, re-enable ingress — with whole-cohort rollback on failure.

## Procedure

Run this from the deploy/orchestration control plane (CI/CD, ECS/K8s
deployment, or scripted `deploy` job). Treat the whole cohort as one unit.

1. **Stage the new config.** Prepare the exact new config (and its target
   `version`) in a staging location the cohort does *not* yet read. Do not write
   it to the live config source yet.

2. **Validate on every member.** Confirm the staged config passes validation for
   the exact image/plugin set each member runs (`config.Validate` / the binary's
   dry-run validation). A config that one member cannot load or validate would
   wedge that member — validate against **all** members before proceeding, not
   just one.

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

## Related

- [Scenario 10: Dynamic Reconfiguration — Cluster Semantics and Limitations](../scenarios/10-dynamic-reconfiguration.md#cluster-semantics-and-limitations)
- [Cluster reconfiguration](cluster-reconfiguration.md) — background on
  per-process reconfiguration and cluster invariants.
- [Config rollback](config-rollback.md) — reverting a committed config change.
- [Node down / failover](node-down-failover.md) — verifying standby takeover.
