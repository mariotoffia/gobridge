# Confirm window — implementation spec (§8.1)

Status: **implemented** · Date: 2026-07-25 · Implements:
[`cluster-config-rollout-protocol.md`](cluster-config-rollout-protocol.md) §8.1,
Q4 · Ships as [ADR 0014](../../adr/0014-confirm-window-provisional-commit.md) ·
Companion to the canonical design (that doc's §8.1 is the authority; this file
records the concrete code-level deltas and the grounding).

Two decisions were finalized during implementation (both grounded, both adversarial-
review-checked):

- **Revert target = the cached pre-swap config, not a re-fetch of the committed
  artifact.** The applier captures `host.Config()` (generation N−1) at the first
  provisional observation and reverts to it. This is always N−1 AND is available for
  the FIRST windowed rollout, which has no prior committed artifact yet (the
  artifact-only approach would leave the first rollout unable to revert). The
  committed artifact still governs a REBOOT during the window (the joiner boots on
  the last confirmed generation).
- **Confirm must land WITHIN the window.** `decideRollout` checks the deadline before
  the confirm barrier, so once the window expires the coordinator reverts even if the
  epoch just converged — matching the members' local deadman and preventing a late
  coordinator from flapping the cohort N−1→N after it had reverted N→N−1. This is
  NETCONF confirmed-commit's "confirmation must arrive before the timer".

The confirm window is opt-in (`confirm_window: 0` default = base protocol,
unchanged). It layers a NETCONF/NSO "provisional apply with deadman timer" on top
of the shipped barrier: on `Committed`, every member swaps **provisionally**; a
member that converges records it; a fenced coordinator writes `Confirmed` once the
whole epoch converged; if `Confirmed` never lands, every member reverts locally to
the last **confirmed** generation (N−1) when its deadline passes.

## Grounding (common wisdom)

- **NETCONF confirmed-commit, RFC 6241 §8.4** — a confirmed commit MUST be reverted
  if no confirming commit arrives within the timeout (default 600 s); a follow-up
  confirmed commit resets the timer; a reboot before confirmation reverts to the
  prior config. Our `confirm_window`, deadman revert, and "reboot onto the last
  confirmed generation" are the same three rules.
  <https://www.rfc-editor.org/rfc/rfc6241.html>
- **Cisco NSO `commit confirmed <timeout>`** — applies provisionally on the device
  and rolls back automatically if confirmation does not arrive. Abort-by-inaction
  is the default outcome; revert reuses the normal apply path.
  <https://nso-docs.cisco.com/guides/operation-and-usage/cli/introduction-to-nso-cli>
- **Q4 (strict all-ack vs Kafka-style fence-non-converged):** stays deferred
  (design §13). Strict revert-the-whole-cohort is simpler and matches the operator
  contract for cohorts ≤ ~10; the confirm window is only the first mechanism that
  makes converged-vs-acked distinguishable, so Q4 is *revisitable* here, not
  *resolved* here.

## Domain (`domain/persistence/rollout.go`, `rollout_snapshot.go`)

- States gain `RolloutConfirmed` (terminal success) and `RolloutReverted`
  (terminal post-commit failure).
- `Rollout` gains `confirmWindow time.Duration` (frozen from the proposal) and
  `confirmDeadline time.Time` (stamped at Commit; zero ⟺ base protocol) and
  `converged map[string]RolloutConverged`.
- `RolloutProposal.ConfirmWindow` carries the window; frozen at Propose.
- `WithCommit(tok, confirmDeadline)` — the store passes `now+confirmWindow` when
  the window is > 0, else the zero time. When zero, `Committed` stays terminal
  exactly as today.
- New transitions:
  - `WithConverged(member, at)` — I6: at most once per member, only while
    `Committed` with an active window; member must be in the epoch. Does not change
    state.
  - `WithConfirm(tok)` — fenced (I3); I7: requires `Committed` + active window +
    every epoch member converged (`CanConfirm`). → `Confirmed`. Idempotent under a
    same-or-newer token.
  - `WithRevert(tok, reason)` — fenced (I3); from `Committed` + active window →
    `Reverted`. Idempotent.
- Terminality moves onto the instance: `Rollout.IsTerminal()` =
  `Aborted|Confirmed|Reverted || (Committed && confirmDeadline.IsZero())`.
  `RolloutState.IsTerminal()` keeps only the always-terminal states
  (`Aborted|Confirmed|Reverted`). All call sites that gate on "no further
  transitions" switch to the instance method.
- Rehydrate invariant reworked: `coordVersion>0 ⟺ state ∈
  {Committed,Aborted,Confirmed,Reverted}` (a decision was recorded). Converged
  members must be epoch members; converged non-empty ⟹ state ∈
  {Committed,Confirmed,Reverted}.

## Ports + stores

- `ClusterRolloutStore` gains `Converge`, `Confirm`, `Revert`. `RolloutProposal`
  gains `ConfirmWindow`. Store `Commit` computes `confirmDeadline` from the frozen
  window + its clock.
- Memory + DynamoDB adapters implement the three; DynamoDB item gains
  `confirmWindow`, `confirmDeadline`, `converged` attributes. "New proposals refused
  while a window is pending" falls out of I1 once the active-check uses
  `Rollout.IsTerminal()` (a windowed `Committed` is non-terminal).
- `storetest.RunClusterRolloutStoreTests` gains: converge-once (I6), confirm needs
  all-converged (I7), revert, fencing on confirm/revert, and window==0 rejecting
  converge/confirm.

## Config

- `ports.ClusterConfig.ConfirmWindow time.Duration`; `config/validate.go` rejects a
  non-zero window without `rollout: coordinated` and a negative window; threaded
  into `RolloutProposal.ConfirmWindow` by the proposer (`rollout_barrier.go`).

## Orchestration (`bridge/`)

- **Coordinator** (`rollout_coordinator.go`): `decideRollout` gains, for a
  `Committed` rollout with an active window — all-converged → `Confirm`; else
  past-`confirmDeadline` → `Revert`; else `Wait`. A crashed coordinator's successor
  drives this (F3), so the row always reaches a terminal state and unblocks the next
  proposal. `coordinatorStep` terminal check uses `Rollout.IsTerminal()`.
- **Applier** (`rollout_applier.go`): poll-based deadman (no goroutine — fits the
  caller-driven model). On `Committed` + active window: provisional swap through the
  existing apply path; each poll, once this member is converged, write `Converge`
  once; if `now > confirmDeadline` and not yet `Confirmed`, **revert locally to N−1
  by re-applying the committed artifact** (which still holds N−1 because the artifact
  now advances only on `Confirmed`). The durable-artifact write moves from adopt to
  the `Confirmed` observation. `Reverted` observation ensures the member is running
  N−1.
- **Convergence signal:** `RolloutHost.Converged(ctx) (bool, error)` — "this member
  has provisionally swapped the committed gen AND its post-swap MQTT-R1 watch is not
  degraded". Implemented by the Supervisor and `bootstrap.App`. Ack proves
  validated+built; Converge proves converged-against-the-real-broker — the whole
  point of the window (research §4 Model A vs B).

## Tests

- Unit: domain state machine (I6/I7, terminality, rehydrate), coordinator
  confirm/revert, applier converge/deadman-revert/confirmed-artifact.
- Conformance: storetest confirm-window cases (memory + DynamoDB Local).
- Integration (ddblocal): happy confirm; deadman revert when a member never
  converges.
- Long-running **UC-CR9**: N=3 real processes, one member never converges → whole
  cohort deadman-reverts to N−1, traffic proof on the reverted generation.

## Non-goals (kept from §8.1)

- No Kafka-style per-member fencing (Q4 deferred).
- No confirm window for base/`refuse` clusters or destructive deltas.
- The cost note stands: for exclusive-identity MQTT sessions a failed trial costs
  two disruptions (apply + revert) — accepted, which is why the window is opt-in.
