# 0014 — Confirm window: provisional commit with deadman revert

Status: accepted
Date: 2026-07-25
Deciders: GoBridge core
Relates to: 0013 (adds an opt-in convergence-gated layer on top of the coordinated
rollout barrier)

## Context

ADR 0013's coordinated commit is final the moment every member has **built** the
candidate (its Ack). One of 0013's stated consequences is "Commit ≠ converged": a
committed config can still fail to converge against the real broker on a node
and is *alarmed, not rolled back*. That is the right default — most
changes should not pay a rollback — but for some changes a syntactically-valid
config that cannot reach its broker (an ACL-denied topic, rotated-away credentials,
an unreachable endpoint) staying active is worse than a second reconnect, and the
operator would rather the cohort **automatically revert** than serve a
non-converging generation until someone intervenes.

The design's split-brain research (NETCONF confirmed-commit RFC 6241 §8.4, Cisco
NSO `commit confirmed`) settles this with a "provisional apply with deadman timer":
apply, then require a confirmation within a window, else revert by inaction. It is
the first mechanism that makes *converged* distinguishable from *acked*.

## Decision

GoBridge adds an **opt-in confirm window** (`bridge.cluster.confirm_window`, a
duration; empty/`0` is the base ADR 0013 protocol). When set, a coordinated commit
is **provisional**:

- Every member swaps the candidate **provisionally** and arms a local deadman timer.
- A member records `Converge` once its post-swap readiness check (every
  non-standby session connected and subscribed) passes.
- The lease-elected, fencing-protected coordinator writes `Confirmed` only when the
  **whole epoch** has converged **and** the window is still open. Confirmation must
  land *within* the window (the deadline is checked before the confirm barrier), so
  a late-resuming coordinator reverts rather than confirming a cohort that has
  already begun to revert.
- If confirmation never lands — any member fails to converge, or the coordinator
  dies — the **whole cohort reverts** to the last confirmed generation (N−1) when
  the window expires: the coordinator writes `Reverted`, and each member's local
  deadman is the backstop for a dead coordinator. Revert re-applies through the
  normal apply path, so it is exercised on every apply, not a special path.
- The durable last-committed artifact advances **only on `Confirmed`**, so a member
  that crashes during the window reboots onto the last *confirmed* generation, never
  the provisional one.

The confirm window keeps every ADR 0013 property: it is opt-in, requires the
versioned DDB config source, applies only to live-safe deltas, and never produces a
mixed-version cohort (revert is whole-cohort, not per-member).

## Consequences

- **Convergence-gated commit for opt-in deltas:** a committed change is permanent
  only after every member reached broker readiness. A config that builds but cannot
  serve reverts the whole cohort instead of latching a per-node degraded alarm.
- **A failed trial costs two disruptions** (apply + revert) on exclusive-identity
  MQTT sessions — which is exactly why it is opt-in and off by default. Use it where
  a non-converging config staying active is worse than a reconnect.
- **Base protocol is byte-for-byte unchanged** when `confirm_window` is unset: the
  commit is final and terminal, exactly as ADR 0013.
- **Reboot during the window reverts:** the provisional generation is never the boot
  config until confirmed (RFC 6241's reboot-reverts rule), so a crash cannot leave a
  member alone on an unconfirmed generation.
- **Strict whole-cohort revert:** the cohort reverts together; no member is fenced
  out of serving while others run the new generation.
- **A revert is not finished when it is decided, and a member that cannot finish it
  is replaced.** The swap back to generation N−1 can fail the same way any apply
  can (a broker refusing the reconnect, a store briefly unopenable), so it is
  retried under a bounded backoff and marked done only once the member is verifiably
  running N−1 again. A member that exhausts the bound is still serving the config
  the cohort rejected and cannot repair itself: it marks itself degraded, publishes
  the generation it could not get back to (`rollout.terminal_generation` in deep
  health, the `ClusterRolloutTerminal` metric), and stops retrying. Replacing it IS
  the repair — it reboots onto the last confirmed generation.

  The mirror-image failure is deliberately NOT treated the same way. A member that
  applied a committed generation but cannot durably record the committed-config
  artifact is running the CORRECT config and only its boot state is stale, so
  replacing it is the one action that would put it on an older generation. It keeps
  retrying at a capped backoff under the same alarm, and retracts it if the store
  comes back. The alarm is shared; the operator action is not, and the reason text
  says which.
- **The local deadman does not depend on the rollout store.** It fires from a
  deadline cached at the provisional swap and runs before the store observation on
  every drive tick, because "no coordinator decided" and "the store stopped
  answering" are the same outage from a member's point of view. Every barrier store
  call is individually bounded and abandoned if it does not return, so a
  black-holed store delays the deadman by at most one tick instead of suppressing
  it.

## Rejected alternatives

- **Kafka-style "commit centrally, fence non-converged members out of serving"**
  (design) — deferred, not adopted. Strict whole-cohort revert is simpler and
  matches the operator contract for cohorts ≤ ~10; the confirm window is merely the
  first mechanism that makes converged-vs-acked distinguishable, so is revisitable
  when cohorts grow, not resolved here.
- **Confirm without a deadman (wait indefinitely for convergence)** — a coordinator
  or member death would wedge the cohort on an unconfirmed generation forever;
  abort-by-inaction is the whole point.
- **Per-member independent confirm/revert** — a member confirming while a peer
  reverts is the mixed-version cohort ADR 0013 forbids; the decision stays with the
  single fenced coordinator over the frozen epoch.
- **Distributed post-commit rollback of an already-*confirmed* generation** — out of
  scope; once confirmed, a later degradation is alarm-and-operate, unchanged from
  ADR 0013 / single-node semantics.
