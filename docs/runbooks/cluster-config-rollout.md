# Runbook: Cluster Config Rollout

**Applies to:** clustered deployments — `deployment_mode: clustered`, or any
instance carrying a static `cluster.endpoints` override — where multiple
instances share a config source, lease store, and outbox/DLQ stores.
**Audience:** operators changing the config of a running cohort.
**Risk:** high — an uncoordinated rollout splits the cohort across config
versions, splitting lease ownership and stranding durable records.

There are **two** paths, and which one applies depends on the change:

- **[Coordinated mode](#coordinated-mode-live-safe-deltas)** (opt-in,
  `cluster.rollout: coordinated` on the versioned DynamoDB config source):
  **live-safe** deltas roll through an all-member barrier with no downtime
  ([ADR 0013](../adr/0013-coordinated-cluster-config-rollout.md)). This is the
  default reading for a coordinated cohort making a live-safe change.
- **[Whole-cohort replacement](#why-live-rollout-is-refused-by-default)** (always
  available, the only path without coordinated mode): stage → drain/stop all →
  commit → start all → verify. Required for **replacement-required** deltas even
  in a coordinated cohort, and for every change in a non-coordinated or
  file-sourced cohort ([ADR 0012](../adr/0012-cluster-config-whole-cohort-replacement.md)).

If you are not sure which class your change is, it is replacement-required until
proven otherwise — see [Which changes are live-safe](#which-changes-are-live-safe).

## Coordinated mode (live-safe deltas)

A coordinated cohort rolls a **live-safe** config change through a staged,
all-member barrier: the change is proposed to a shared store as a candidate
generation over a frozen membership roster; every member validates and builds it
and acknowledges; a lease-elected coordinator commits only once every member has
acknowledged; and members swap **only** after that store-atomic commit. No
supported path produces a mixed-version cohort, and a rejected change costs an
abort (the running config keeps serving), not an outage.

### Enabling it

Coordinated mode is off by default (a cohort keeps the whole-cohort refusal). To
enable it, ALL of the following must hold:

- **Config source:** the versioned, CAS-capable DynamoDB config source (the
  `dynamodb_coordinated_ha` profile). An EFS/file-sourced cohort cannot use
  coordinated mode — it keeps whole-cohort replacement.
- **Logical config:** set `bridge.cluster.rollout: coordinated` and list the cohort
  in `bridge.cluster.members` (the roster the barrier freezes as its membership
  epoch — NOT `cluster.endpoints`, which is a capability map).
- **Per-node identity:** each task's deployment config must set a **stable**
  `member_id` (bootstrap `member_id`) that appears verbatim in
  `bridge.cluster.members`. It must survive restarts — it is the identity a
  restarted task rejoins the cohort under. A coordinated boot with an empty or
  unlisted `member_id` **fails to start**, loudly, rather than silently never
  acknowledging.

With those in place the shipped file-based image performs coordinated rollouts
itself; there is nothing to run per-change beyond posting the config.

### Performing a live-safe change

1. **Post the change to the config source** (the same durable write you already
   make — a single node, or the admin commit API). Do NOT stage-and-stop; that is
   the whole-cohort path.
2. Every member observes the change through its own config source, classifies it,
   and — if live-safe — **proposes** it to the barrier and **defers** its local
   swap. Health reports the change as pending (see below).
3. When every member has acknowledged, the coordinator commits and each member
   swaps to the new generation. Convergence is then per-node exactly as for a
   single-node reload (commit ≠ converged; a committed config can still degrade on
   a node and is alarmed, not rolled back).

### Observing a rollout

Deep health (`GET /api/v1/monitor/deephealth`, `config_watch.rollout`) surfaces the
barrier as this member last saw it: `member_id`, `generation`, `state`
(`proposed` | `staging` | `committed` | `aborted`), `config_version`, the frozen
`epoch`, and `acked` / `nacked`. **Epoch minus acked minus nacked is exactly who
the cohort is waiting for** — a rollout that stalls names the member holding it up
(most often one whose own config source has not yet delivered the candidate).
`config_watch.reconfigure_pending` is expected true for the duration of an
undecided rollout: this member has deliberately not applied a config the cohort
has not committed.

### When a rollout aborts or stalls

- **Abort** (a member Nacked the candidate as unbuildable, or the coordinator hit
  the rollout deadline): nothing swaps, every member keeps its running config, and
  `state` reads `aborted` with a `reason`. Fix the config and re-post it, or leave
  it — the cohort is unharmed.
- **Restart during or after an abort:** a member that restarts while its config
  source still holds a candidate the barrier has not committed boots on the last
  **committed** config, not the rejected candidate — it never joins the cohort on a
  config no peer runs. To clear a lingering `reconfigure_pending`, roll the config
  source back to the last committed document (or fix and re-propose the change).

### Which changes are live-safe

The barrier admits only **live-safe** deltas — those that pass the same per-node
reload preflight a single-node live reload already requires. Everything else is
**replacement-required** and keeps the whole-cohort procedure below, even in a
coordinated cohort:

| Class | Examples | Path |
|---|---|---|
| **live-safe** | routing/binding changes, processor tuning, log level, non-identity session options, adding/removing a non-durable route | Coordinated barrier (no downtime) |
| **replacement-required** | changing a durable session identity (client id, subscription); changing a lease / outbox / DLQ **store target**; changing `deployment_mode` | Whole-cohort replacement |
| **replacement-required (cohort shape)** | changing `bridge.cluster.members` (the roster/epoch), `bridge.cluster.endpoints`, or `bridge.cluster.rollout` itself | Whole-cohort replacement |

The last row is the one operators most often miss: a coordinated cohort **cannot
roll a change to its own membership roster, endpoint map, or rollout mode through
the barrier** — the roster is the epoch the barrier freezes and counts
acknowledgements against, so changing it is structurally a whole-cohort
replacement. The barrier refuses such a delta up front (it is never proposed), with
a message naming the class and pointing here.

## Why live rollout is refused by default

**This section describes the default (non-coordinated) behavior.** A cohort that
has enabled [coordinated mode](#coordinated-mode-live-safe-deltas) rolls live-safe
deltas through the barrier instead; the refusal below still applies to a
non-coordinated cohort, a file-sourced cohort, and every replacement-required delta.

Reconfiguration is **per-process**. Each instance watches its own config source,
validates, and swaps its runtime independently. Without the coordinated barrier
there is **no cluster-wide version barrier, no all-member readiness gate, and no
coordinated rollback**
([Scenario 10](../scenarios/10-dynamic-reconfiguration.md#clustered-live-reload-is-rejected-fail-closed)).

Because of that, the runtime **refuses every non-no-op live reload of (or into) a
clustered deployment, fail-closed** unless coordinated mode is enabled (finding H8). The guard runs in both reload
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
