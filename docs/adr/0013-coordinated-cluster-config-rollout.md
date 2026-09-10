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
observing Committed. The protocol is opt-in (`bridge.cluster.rollout: coordinated`),
requires the versioned DDB config source, and applies only to deltas passing the
live-safe preflight — durable-identity and store-target changes keep 0012's
whole-cohort replacement.

The barrier is runtime-hosted: `bridge.Supervisor` and the shipped file-based
`bootstrap.App` both drive it through a `bridge.ClusterRolloutDriver` bound to a
`ports.RolloutHost` seam (the host is a port, not a bridge type), so the shipped
image performs coordinated live-safe changes rather than the ADR 0012 refusal.

### What the barrier guarantees, precisely

The guarantee is **atomic before the commit, per-member after it**. Both halves
are load-bearing and neither implies the other:

- **Pre-commit barrier (atomic).** No member applies anything until the
  store-atomic Commit, and that Commit requires every epoch member's ack. So the
  *decision* is a single event with a single outcome, and there is no supported
  path by which some members apply a generation the cohort never agreed on. A
  member that was down, that never staged the candidate, or that restarted
  mid-rollout is covered by the same rule: the joiner refuses to boot a config
  ahead of the barrier, and boot resolution recovers the last committed artifact.
- **Post-commit convergence (per-member, eventual).** Applying the committed
  generation is local work — building stores, opening transports, reconciling
  subscriptions — and it can fail on one member while succeeding on another. The
  cohort is therefore **not** instantaneously uniform after a commit: there is a
  convergence window in which one member still runs generation N-1 while its peers
  run N.

That window is what makes the protocol usable at all — the alternative is a
distributed post-commit rollback, which needs the same barrier again with traffic
in flight. It is made safe not by being impossible but by being **bounded and
visible**:

- A member whose swap fails retries it, and keeps retrying at a capped backoff
  rather than giving up: most causes (a broker refusing one connection, a store
  briefly unopenable) clear on their own, and a member that stopped trying would
  be a permanently split cohort nobody chose.
- Past a small attempt bound it declares itself **terminal**: it cannot reach the
  cohort's decision on its own, and replacing it is the repair (a replacement
  boots on the committed artifact).
- Throughout, the member publishes `applied` (is it running the decided
  generation), `confirm_pending` (is that decision even final), `observed_at` /
  `stale` (is the answer current), and `terminal_generation` /
  `terminal_reason` in `/deephealth`, plus the `ClusterRolloutDiverged`,
  `ClusterRolloutTerminal` and `ClusterRolloutObservationAge` metrics. A
  deployment MUST alarm on the fleet maximum of those three; the shipped CDK
  bundle installs them (`EnableClusterRolloutAlarms`).

A **provisional** commit — one carrying a confirm window — is deliberately not
counted as divergence by any of these, because it has decided nothing yet: the
window itself handles a member that cannot converge, by reverting the cohort.

Callers that need a *converged* cohort rather than a *committed* one use the
opt-in confirm window in ADR 0014, which gates the final Confirm on every member
recording post-swap convergence and reverts the whole cohort otherwise.

## Consequences

- No supported path applies a generation the cohort did not agree on: members swap
  only after a store-atomic Commit that required every epoch member's ack.
- Commit ≠ converged. A committed config can still fail to apply or to reach its
  broker on one node; that node runs the previous generation until it converges
  or is replaced, and it is alarmed rather than rolled back. Mixed generations are
  a bounded, observable state, not an impossible one.
- Coordinator and member crashes resolve via lease TTL + rollout deadline; all
  protocol state is durable, so recovery is resumption, not repair.
- A deployment that cannot tolerate any mixed-generation window must either use
  the ADR 0014 confirm window (which reverts the cohort instead of leaving it
  split) or keep 0012's whole-cohort replacement.
- EFS/file-sourced clusters and destructive deltas keep ADR 0012.

## Rejected alternatives

- Node-to-node RPC coordination — new failure surface; the store bus already
  carries leases/outbox safely.
- External consensus (raft/etcd) — operational dependency out of proportion to one
  barrier.
- Rolling per-node apply with eventual convergence and **no barrier** — an
  unbounded mixed-version window with no agreed decision point (0012's original
  rejection stands). Note the difference from what is decided above: the barrier
  makes the *decision* atomic; only the *application* is per-node, and only after
  every member has already proven it can build the candidate.
- Distributed post-commit rollback as the default — requires the same barrier
  again with traffic in flight; alarm-and-operate matches the existing model, and
  0014 offers the gated variant for the changes that warrant it.
