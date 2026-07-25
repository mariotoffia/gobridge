# 0013 — Coordinated cluster config rollout for live-safe deltas

Status: accepted
Date: 2026-07-25
Deciders: GoBridge core
Supersedes: 0012 (for live-safe deltas only)
Extended by: 0014 (opt-in confirm window: provisional commit with deadman revert)

## Context

ADR 0012 refuses every non-no-op clustered live reload because the cluster had no
version barrier, readiness gate, or coordinated rollback. The refusal was safe but
forces orchestration downtime for every change, including trivially safe ones.

## Decision

GoBridge implements a staged, all-member barrier protocol over the shared store: an
operator posts a change to any node; it is proposed as a candidate generation with
a frozen membership epoch; every member validates and builds it, then acks; a
lease-elected, fencing-protected coordinator commits only when acks cover the
epoch, else aborts (timeout, Nack, membership change). Members swap only on
observing Committed. Post-commit convergence remains per-node (MQTT-R1). The
protocol is opt-in (`cluster.rollout: coordinated`), requires the versioned DDB
config source, and applies only to deltas passing the live-safe preflight —
durable-identity and store-target changes keep 0012's whole-cohort replacement.

The barrier is runtime-hosted: `bridge.Supervisor` and the shipped file-based
`bootstrap.App` both drive it through a `bridge.ClusterRolloutDriver` /
`bridge.RolloutHost` seam, so the shipped image performs coordinated live-safe
changes rather than the ADR 0012 refusal.

## Consequences

- No supported path can produce a mixed-version cohort: members swap only after a
  store-atomic Commit that required every epoch member's ack.
- Coordinator and member crashes resolve via lease TTL + rollout deadline; all
  protocol state is durable, so recovery is resumption, not repair.
- Commit ≠ converged: a committed config can still degrade per node and is alarmed,
  not rolled back — unchanged from single-node semantics.
- EFS/file-sourced clusters and destructive deltas keep ADR 0012.

## Rejected alternatives

- Node-to-node RPC coordination — new failure surface; the store bus already
  carries leases/outbox safely.
- External consensus (raft/etcd) — operational dependency out of proportion to one
  barrier.
- Rolling per-node apply with eventual convergence — unbounded mixed-version window
  (0012's original rejection stands).
- Distributed post-commit rollback — requires the same barrier again with traffic
  in flight; alarm-and-operate matches the existing model.
