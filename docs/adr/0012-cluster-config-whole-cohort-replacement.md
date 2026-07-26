# 0012 — Cluster config changes require whole-cohort replacement

Status: accepted
Date: 2026-07-16
Deciders: GoBridge core
Supersedes: 0007
Superseded by: 0013 (for live-safe deltas only)

> **Superseded by [0013](0013-coordinated-cluster-config-rollout.md) for live-safe
> deltas.** A coordinated cohort (`cluster.rollout: coordinated` on the versioned
> DynamoDB config source) now rolls live-safe deltas through an all-member barrier
> instead of refusing them. This ADR still governs everything else: a
> non-coordinated cluster, an EFS/file-sourced cluster, and any
> replacement-required delta (durable-identity or store-target change) keep the
> whole-cohort replacement procedure below.

## Context

Each GoBridge process watches, validates, and applies configuration
independently. The cluster has no config-version consensus, all-member readiness
gate, or coordinated rollback. A live per-process rollout could therefore leave
members on different route, store, session, and policy definitions
indefinitely.

Lease fencing cannot make that split safe. In particular, changing a session
identity or lease/outbox store target can create independent active drainers or
strand durable records. A local config-version CAS only serializes writes to a
shared config source; it does not coordinate application across the cohort.

ADR 0007 selected `AdoptValid` so read-only workers would not overwrite valid
Admin-API changes on shared EFS. That remains a safe startup seeding rule, but
the ADR incorrectly treated adoption as a live cluster rollout mechanism.

## Decision

GoBridge rejects every non-no-op live reload **of or into** a clustered
deployment, fail-closed.

- Both `bridge.Supervisor.apply` and
  `bootstrap.App.applyLogicalConfig` enforce the rule after no-op detection and
  before build, store inspection, drain, or stop.
- Rejection leaves the current runtime and running config version unchanged and
  reports failure through the existing reload/apply diagnostics.
- `WithAllowDestructiveReload` does not bypass the guard. Local backlog
  deletion cannot replace cluster coordination.
- Byte-identical watcher re-emits remain accepted no-ops.
- `AdoptValid` remains the read-only worker startup policy. It only controls
  which already-committed config a freshly starting worker adopts.

A clustered config change is an externally coordinated whole-cohort
replacement:

1. Stage the exact config and validate it against every member's image/plugins.
2. Quiesce ingress, drain all work, and stop every member.
3. Commit the staged config while the cohort is down.
4. Start every member.
5. Re-enable ingress only after every member reports the target
   `config_version` and passes the Full/readiness barrier.

Any failure keeps ingress quiesced and rolls the entire cohort back to the
previous config. The normative procedure is
`docs/runbooks/cluster-config-rollout.md`.

## Consequences

- A live or rolling cluster config change cannot produce a split-version cohort
  through supported reload paths.
- Cluster config changes require orchestration downtime unless a future control
  plane supplies consensus, coordinated readiness, and rollback.
- An Admin-API commit can durably change the shared file while its live apply is
  rejected. Operators must not use that as a rollout shortcut; they must follow
  the whole-cohort procedure.
- Standalone live reload behavior is unchanged.
- Startup seeding and runtime reconfiguration are separate decisions:
  `AdoptValid` prevents workers from overwriting shared config, while this ADR
  governs when clustered config may become active.

## Rejected alternatives

- **Independent live reload with eventual convergence.** Rejected because a
  failed or delayed member leaves an unbounded mixed-version window.
- **Permit only changes believed version-skew-safe.** Rejected because the
  runtime has no cluster barrier proving every member applies or rolls back the
  same change.
- **Use `BridgeConfig.Version` as a barrier.** Rejected because it is local
  optimistic concurrency, not distributed consensus or an apply acknowledgement.
- **Bypass with destructive reload.** Rejected because deleting local durable
  state does not coordinate ownership or preserve records across the cohort.
