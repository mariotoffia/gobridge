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
  acked), and the abort reason repeats it: `rollout deadline exceeded with 1/3
  acks; never voted: gobridge-ha-worker-1, gobridge-ha-worker-2`.
- Then read `config_watch.rollout.not_voting` **on each named member**. That is
  the only place the cause lives — a member that never voted leaves no trace in
  the shared row — and it distinguishes the three cases, which need different
  actions:

  | `not_voting` says | What happened | What to do |
  |---|---|---|
  | its own config source has not delivered the candidate | the benign case: that member's watcher is lagging | wait; the deadline bounds it. If it never arrives, check that member's config source. |
  | its barrier refused to carry the delta | that member read a **different document** from the proposer's, so it computes a different candidate identity and cannot join the rollout | reconcile the config sources; the reason names both digests. |
  | it is not in the frozen membership epoch | the roster and the member's identity disagree | fix `bridge.cluster.members` or the member's id, then re-post. |

- A rollout that cannot gather every acknowledgement **aborts on its own at its
  deadline**; the running config keeps serving. Fix the config and re-post it, or
  roll the config source back to the last committed document.
- A member that restarts while the source still holds an uncommitted candidate
  boots on the last committed config — see
  [Operating a coordinated cohort](../cluster/operating.md#when-a-change-doesnt-go-through).

## The generation-zero baseline

A coordinated cohort recovers a restarting member to the config the cohort last
**committed**. Before the very first rollout commits there is no such record, so
the deployment establishes one at startup instead.

- The deployment stamps the digest of the exact config document it seeds
  (`dynamodb_ha_baseline_config_digest` in bootstrap). When a member boots on
  precisely that document, it records it as the cohort's **generation zero** and
  verifies the write by reading it back, before it starts serving.
- The record is written only after this member has built and installed that
  config, so a config a member cannot run never becomes the cohort's baseline.
- Every member of the cohort writes the same baseline; that is a no-op, not a
  conflict. Once any rollout has committed, the baseline write is ignored — it can
  never rewind the cohort to its deploy state. If a **different** baseline is
  already established (a peer's, or an earlier deploy's), that one wins: this
  member adopts and reports it instead of overwriting it.
- After that, a member restarting while the config source still holds a change
  the cohort has not committed boots the **committed** config and offers the
  change to the barrier, instead of running a config no peer is running.

Read it in deep health under `config_watch.rollout`: `baseline_generation` and
`baseline_digest` are what a restart of that member would recover to.

Read the `cluster_rollout_baseline_seed` audit event to see what a member did:

| Outcome | Meaning |
|---|---|
| `verified` | This member established the baseline, or re-wrote the identical one, and read it back. |
| `superseded` | A different baseline was already established; this member adopted it. Expect this on a redeploy whose config changed — the change then rolls through the barrier as usual. |
| `adopted` | This member's document is not the deployment baseline (normal after a committed change); it reports the baseline that stands. |
| `skipped` | Not the deployment baseline **and** no baseline exists yet — the conservative joiner rule applies, as before any seed. |
| `failed` | The rollout store could not be written or read. Startup fails; this is a store outage, not a config problem. |

If members disagree about which document is the baseline for long, they were
deployed from different config documents. Redeploy the cohort from one.

## Related

- [Operating a coordinated cohort](../cluster/operating.md) — the no-downtime
  flow for live-safe changes and how to read a rollout's health.
- [Cluster configuration guide](../cluster/README.md) — choosing a cluster mode.
- [Scenario 10: Dynamic Reconfiguration — Cluster Semantics and Limitations](../scenarios/10-dynamic-reconfiguration.md#cluster-semantics-and-limitations)
- [Cluster reconfiguration](cluster-reconfiguration.md) — background on
  per-process reconfiguration and cluster invariants.
- [Config rollback](config-rollback.md) — reverting a committed config change.
- [Node down / failover](node-down-failover.md) — verifying standby takeover.
