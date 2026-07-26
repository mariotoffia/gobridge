# Design: Coordinated cluster config rollout (barrier protocol)

Status: **fully implemented** — every slice has shipped. This is the canonical
protocol design. The shipped decisions are recorded authoritatively in
[ADR 0013](../../adr/0013-coordinated-cluster-config-rollout.md) (base barrier)
and [ADR 0014](../../adr/0014-confirm-window-provisional-commit.md) (confirm
window); where this doc and an ADR ever disagree, the ADR wins. §8.1 (the
confirm window) is the final piece of the protocol — there is nothing beyond it.

The barrier is wired into **both** runtime hosts — `bridge.Supervisor` and the
shipped file-based `bootstrap.App` — through a `bridge.ClusterRolloutDriver`
bound to a `ports.RolloutHost` seam (the host is a *port*, not a bridge type),
so a coordinated cohort performs live-safe rollouts and a non-coordinated one
keeps the ADR 0012 refusal.

Companion docs in this folder:
[`cluster-config-rollout-confirm-window.md`](cluster-config-rollout-confirm-window.md)
(confirm-window code-level detail) and
[`cluster-config-rollout-research.md`](cluster-config-rollout-research.md)
(prior art & split-brain analysis — the external specs this design reuses).

Date: 2026-07-26 · Relates to: ADR 0012, ADR 0013, ADR 0014,
`PROD_READY_ISSUES.md` §3 CLUSTER-3 · Operations:
[`docs/runbooks/cluster-config-rollout.md`](../../runbooks/cluster-config-rollout.md)
· Plain-language configuration guide: [`docs/cluster/README.md`](../README.md)

---

## 1. Problem

Each GoBridge process watches, validates, and applies configuration
independently. There is no cluster-wide version barrier, no all-member
readiness gate, and no coordinated rollback — so the runtime refuses every
non-no-op live reload of (or into) a clustered deployment, fail-closed
(ADR 0012, guard in `bridge.Supervisor.apply` and
`bootstrap.App.applyLogicalConfig`). Operators must replace the whole cohort.

Goal: allow an operator to POST a config change to **any one node** and have
the cluster apply it **atomically-in-effect**: every member stages it as a
candidate, and only when **all members acknowledge** does it commit; any
failure aborts everywhere and the old config keeps serving.

## 2. Goals and non-goals

Goals

- G1 One-shot operator flow: `POST` → propagate → all-ACK → commit.
- G2 No mixed-version cohort through any supported path (0012's invariant kept).
- G3 Crash-safe: coordinator or member death mid-rollout never wedges or
  splits the cluster; the protocol resolves to Committed or Aborted.
- G4 Reuse the existing coordination substrate (shared store, conditional
  writes, lease fencing) — **no node-to-node RPC**.
- G5 Deterministically testable with the existing harness (`nodeProcess`
  multi-process launcher, `wait` gates, ddblocal).

Non-goals

- N1 General consensus (no raft/etcd dependency). The shared store is the
  single source of truth; DynamoDB conditional writes provide the atomicity.
- N2 **Destructive deltas stay out of scope.** Changes that alter durable
  identity or store targets (session identity, lease/outbox/DLQ store
  bindings — the reasons 0012 exists) are still refused live and keep the
  whole-cohort replacement procedure. The protocol lifts refusal only for
  deltas that pass the existing per-node preflight (see §8 rollout classes).
- N3 File/EFS-sourced clusters. The protocol requires the versioned,
  CAS-capable DDB config source; EFS-file clusters keep ADR 0012 unchanged.
- N4 Per-topic partial service (MQTT-RES-1) — unrelated, stays as is.
- N5 Post-commit distributed rollback. Commit means "every member validated
  and built the candidate"; per-node convergence after swap remains guarded
  by the existing MQTT-R1 watch (`ConfigDegraded`), same as single-node.

## 3. Protocol overview

```
operator ── POST config ──► any node (existing admin txn API)
                              │  Propose: conditional-create rollout row
                              │  gen=N, digest, membership epoch snapshot
        every member ─────────┤  sees candidate (store watch)
                              │  preflight class check → validate → BUILD
                              │  candidate runtime (existing prepare path)
                              │  → Ack(gen, member)   (or Nack(reason))
        coordinator ──────────┤  (holder of the rollout lease, fenced)
                              │  acks == epoch set → Commit(gen)   [atomic flip]
                              │  Nack / timeout / epoch change → Abort(gen)
        every member ─────────┘  observes Committed → swap (prepare→commit)
                                 observes Aborted   → discard candidate
                                 post-swap: MQTT-R1 convergence watch as today
```

States: `Proposed → Staging → Committed | Aborted` (terminal). One active
rollout at a time; generation is monotonic. The confirm window (§8.1) adds two
further terminal states, `Confirmed | Reverted`, reached only when a window is
set.

## 4. Domain model (Layer 1)

Placement: **`domain/persistence`** — the rollout barrier is the same family
as leases and outbox claims (coordination-through-durable-state, fencing,
TTL); it reuses `persistence.LeaseToken` for fencing. A separate
`domain/coordination` context was considered and rejected: it would hold two
value types and force a new arch component + lint mapping for no added
invariant clarity.

New value types (no behavior beyond invariant checks):

```go
type RolloutState string // Proposed | Staging | Committed | Aborted

type Rollout struct {
    Generation      uint64        // monotonic, one active at a time
    State           RolloutState
    ConfigDigest    string        // sha256 of canonical candidate config
    ConfigVersion   int           // the BridgeConfig.Version being rolled out
    MembershipEpoch []string      // sorted member IDs frozen at Propose
    Acks            map[string]RolloutAck
    Reason          string        // abort reason / nack aggregation
    Deadline        time.Time     // coordinator aborts after this
}

type RolloutAck struct { MemberID string; BuildDigest string; At time.Time }

type RolloutProposal struct {
    ProposerID    string
    ConfigDigest  string
    ConfigVersion int
    Members       []string      // epoch snapshot
    TTL           time.Duration // rollout deadline budget
}
```

Invariants (enforced by domain constructors + store conditions):

- I1 `Propose` only when no rollout is in `Proposed|Staging`.
- I2 `Commit` only when `State==Staging ∧ keys(Acks) == MembershipEpoch`.
- I3 `Commit`/`Abort` only with a fencing token ≥ the recorded coordinator
  token. **Scope:** the recorded token is written *by* a decision, so it is
  zero until the first one — I3 rejects a stale **re-**decision, not a stale
  first decision (see F5).
- I4 Terminal states never transition.
- I5 A member acks at most once per generation; ack after `Aborted` is a
  no-op error.

The confirm window (§8.1) adds I6 (a member converges at most once, only under
an active window) and I7 (`Confirm` requires the whole epoch converged).

## 5. Ports (Layer 2 boundary)

One new small port (hexagonal; interface stays minimal per project rule):

```go
// ClusterRolloutStore coordinates a staged, all-member config rollout
// through durable conditional writes. Implementations must make Propose,
// Commit, and Abort atomic check-and-set operations.
type ClusterRolloutStore interface {
    Propose(ctx context.Context, p persistence.RolloutProposal) (persistence.Rollout, error)
    Ack(ctx context.Context, gen uint64, memberID, buildDigest string) error
    Nack(ctx context.Context, gen uint64, memberID, reason string) error
    Commit(ctx context.Context, gen uint64, tok persistence.LeaseToken) error
    Abort(ctx context.Context, gen uint64, tok persistence.LeaseToken, reason string) error
    Current(ctx context.Context) (persistence.Rollout, error)
    // confirm window (§8.1): Converge, Confirm, Revert
}
```

Reused, unchanged:

- **Coordinator election** — existing `ports.LeaseStore` on a well-known
  lease ID (`cluster/rollout-coordinator`); its `LeaseToken` is the fencing
  token passed to `Commit`/`Abort` (I3). No new election code.
- **Membership** — the epoch snapshot at Propose comes from the static
  `cluster.members` roster (§8), frozen at Propose (§7 F6).
- **Config transport** — the rollout row stores only digest+version; each
  member's own config source delivers the candidate bytes and the digest is the
  cross-member agreement check (§11 records why, and the joiner rule that makes
  it safe).

Adapters (Layer 3): `adapter_store_dynamodb_rollout` (DynamoDB, conditional
writes — same idioms as `dynamodblease`), `adapter_store_memory_rollout`
(tests / single-binary). Both register per `PLUGIN.md`; both must pass the
`ports/storetest.RunClusterRolloutStoreTests` conformance suite.

**Store invariants (split-brain prevention by construction — research §3):**
coordination tables (rollout, lease) are **single-Region** (multi-AZ
tolerance is inherent: DynamoDB replicates per-partition across AZs with
internal Paxos). They must **never become MREC global tables** — cross-Region
last-writer-wins would let two "successful" conditional writes diverge,
reintroducing split-brain at the store. All rollout decision reads use
`ConsistentRead`; never GSIs/Streams. Under these invariants, divergent
histories needing "merge" cannot exist: concurrent proposals resolve to one
CAS winner and one `ConditionalCheckFailedException`.

## 6. Orchestration (Layer 2, `bridge/`)

Proposer (any node, from the existing admin config-transaction API): commit
of a txn in coordinated-cluster mode writes the candidate config to the
config source (inactive), computes the digest, and calls `Propose`.

Applier (every node, in the Supervisor): on observing `Proposed/Staging`
with an unseen generation — run rollout-class preflight (§8); fetch + digest-
verify candidate; validate; **build a candidate runtime via the existing
`applyPrepareCommit` prepare path but do not swap**; `Ack` (or `Nack` with
the error). Then wait: on `Committed` → complete the prepared swap (post-swap
MQTT-R1 watch runs as today); on `Aborted` → discard the candidate runtime
(existing candidate-cleanup path, RECONFIG-2). Store notifications are
hints, never truth: every decision re-reads the rollout row with
`ConsistentRead` (research rule 11). **Each node is itself a token-checking
resource** (research §3): it persists the highest `(epoch, generation)` it
has applied and rejects anything lower — a paused-then-resumed
coordinator's late push is harmless at the node, not just at the store.

Coordinator (whichever node holds the rollout lease): poll `Current` +
membership; `Commit` when acks cover the epoch; `Abort` on any `Nack`, on
`Deadline` expiry, or on membership-epoch change. Coordinator work is
idempotent and resumable — all state is in the store, so a successor elected
after a crash continues from whatever it reads. A freshly elected
coordinator **waits out one full previous-lease duration before its first
side effect** (Chubby-style lock-delay — belt and braces over the fencing
epoch; DynamoDB itself does this internally for its partition leaders).

Joiner rule: a starting member adopts only the last **Committed**
configuration (unchanged `AdoptValid` semantics); it never acks a rollout
proposed before it joined (its ID is not in the epoch).

Guard change: the ADR 0012 refusal remains the **default**. It is lifted
only when `bridge.cluster.rollout: coordinated` is configured **and** the delta
is live-safe (§8); everything else still refuses fail-closed exactly as today.

## 7. Failure matrix

| # | Failure | Outcome | Mechanism |
|---|---|---|---|
| F1 | Member crashes before Ack | Rollout aborts at deadline; survivors keep old config; member rejoins on old (still-committed) gen | Deadline + joiner rule |
| F2 | Member Nacks (validation/build fails) | Abort everywhere; candidates discarded | Coordinator on first Nack |
| F3 | Coordinator crashes mid-rollout | Rollout lease expires (TTL); successor resumes from store state and commits/aborts | LeaseStore election + idempotent coordinator |
| F4 | Two concurrent proposes | Second `Propose` fails the conditional create (I1) | CAS |
| F5 | Deposed coordinator tries Commit/Abort **after the live one decided** | Rejected — stale fencing token (I3) | Token condition |
| F5b | Deposed coordinator decides **first** (no decision recorded yet) | **Not fenced** — accepted. Fail-safe: a zombie Commit still needs the full ack barrier (I2), a zombie Abort just keeps the old config serving. Bounded in practice by the successor's lock-delay (§6) | Residual — accepted (§11) |
| F6 | Membership changes mid-rollout (join/leave) | Abort; operator retries (cheap — nothing swapped) | Strict epoch equality; simplest safe rule |
| F7 | Member crashes after Commit, before its swap | Rejoins and boots the committed gen — same config, no split | Joiner rule |
| F8 | Committed config fails to converge on a node (e.g. broker unreachable) | No distributed rollback; that node latches `ConfigDegraded` via MQTT-R1, alarmed — parity with single-node behavior (unless a confirm window is set, §8.1) | §2 N5 |
| F9 | Store unavailable mid-rollout | No state flips possible; members keep old config; rollout resolves (or deadline-aborts) when the store returns | All transitions are store writes |
| F10 | Candidate bytes tampered / mismatched | Member digest check fails → Nack → abort | Digest in rollout row |

## 8. Rollout classes and config surface

Per-node preflight classifies the delta before Ack:

- **live-safe** — passes today's reload preflights (no durable session
  identity change, no lease/outbox/DLQ store target change, no
  deployment-mode change). Eligible for coordinated rollout.
- **replacement-required** — anything else. `Nack(reason=class)` → abort with
  a message pointing at the whole-cohort runbook. ADR 0012 continues to
  govern these.

New config keys (validated in `config/validate.go`):

<!-- docs-example: skip -->
```yaml
bridge:
  deployment_mode: clustered
  cluster:
    rollout: refuse | coordinated   # default: refuse (today's behavior)
    members: [node-a, node-b]       # required when coordinated: the cohort roster
```

`members` is the membership epoch the barrier freezes (F6) — NOT
`cluster.endpoints`, which is this instance's capability map. `coordinated` also
requires a versioned, CAS-capable config source and a wired rollout store; those
are composition-root wiring, invisible to the blueprint validator, so a root that
wires no rollout store makes every coordinated reload fail closed. Each process
announces its own `member_id` (bootstrap config), which must appear in `members`
and survive restarts.

### 8.1 Confirm-window extension (Model B — opt-in, ✅ implemented)

The base protocol's `Ack` proves *validated + built*, not *converged against
the real broker* (research §4, Model A vs B). An opt-in confirm window adds
the NETCONF/NSO "provisional apply with deadman timer" layer on top
(research rules 8 and 10):

<!-- docs-example: skip -->
```yaml
bridge:
  cluster:
    rollout: coordinated
    confirm_window: 90s     # 0 (default) = base protocol only
```

- On `Committed`, every node performs its swap **provisionally**, arming a
  local deadman timer of `confirm_window`.
- Each node that reaches convergence (the MQTT-R1 readiness check — the
  NMDA "intended vs operational" instrument) writes a `Converged` record.
- The coordinator writes `Confirmed` (fenced CAS) when all epoch members
  converged; nodes observing `Confirmed` disarm their timers.
- If `Confirmed` never appears — any node failed to converge, or the
  coordinator died — **every node reverts locally when its timer fires, by
  re-applying generation N−1 through the exact same apply pipeline**.
  Abort is the default outcome, achieved by inaction (the NSO crux); revert
  is not a special path, so it is exercised every time apply is (research
  rule 9). A crashed node reboots onto the last *confirmed* generation —
  the provisional generation is never the boot config until confirmed
  (RFC 6241's reboot-reverts rule).
- While a confirm window is pending, new proposals are refused (research
  rule 12 — snapshot revert must not wipe concurrent changes).

Cost note (why this is opt-in): for exclusive-identity MQTT sessions a
failed trial costs **two** service disruptions (apply + revert) — the JunOS
trade, accepted when a bad config staying active is worse. See
[`cluster-config-rollout-confirm-window.md`](cluster-config-rollout-confirm-window.md)
for the code-level detail and the two implementation decisions (revert target =
cached pre-swap config; confirm must land within the window).

## 9. Observability

- Metrics: `ClusterRolloutState` gauge (per state, including
  `confirmed`/`reverted`), `ClusterRolloutAcks` gauge (`acks/epoch`),
  `ClusterRolloutResolved` counter (`outcome=committed|aborted|…`, `reason=…`).
- Deep health: a `rollout` section (generation, state, acked members,
  whether this member converged).
- Runbook: `cluster-config-rollout.md` gains a "coordinated mode" chapter;
  the whole-cohort procedure stays as the replacement-required path.

## 10. Test plan (all gates deterministic — TESTS.md rules)

Unit (`domain/persistence`, `bridge/`):
- Rollout state machine: table-driven transitions + every invariant I1–I7.
- Rollout-class decision: table of deltas → live-safe/replacement-required.
- Coordinator logic on `clocktest` (deadline abort, resume-from-store,
  stale-token rejection, confirm/revert) with fake stores.

Conformance (`ports/storetest`):
- `RunClusterRolloutStoreTests`: CAS races (double propose, commit vs abort
  race, stale fencing token, ack-after-abort), plus confirm-window cases
  (converge-once, confirm-needs-all-converged, revert, fencing), run against
  memory + DynamoDB implementations (mandatory per TESTS.md §3.3).

Integration (`tests/integration/`, ddblocal + mqttlocal):
- Single-process happy path: propose → ack → commit → swap through the real
  Supervisor.
- Abort path: Nack member (invalid plugin) → nothing swaps, old config
  serves, `ClusterRolloutResolved{outcome=aborted}` emitted.
- Restart onto the last committed config (durable artifact, real codec).
- Confirm-window confirms (real convergence + artifact advances).

Long-running (`tests/longrunning/`, `nodeProcess` multi-process harness —
real processes, real DynamoDB, real broker; barriers are stdout tokens +
store rows, never sleeps):
- UC-CR1: N=3 happy path — all nodes swap; equal `config_version`; traffic
  proof before and after (also UC-CR7: cross-member digest agreement).
- UC-CR2/CR3: SIGKILL a member → deadline abort + rejoin on committed gen;
  a killed coordinator is succeeded by a live member that drives the abort.
- UC-CR9: confirm window confirms across three real processes.

## 11. Implementation status, decisions & coverage

Every slice has shipped and lands green on `make lint` + `make test`. The base
barrier is [ADR 0013](../../adr/0013-coordinated-cluster-config-rollout.md); the
confirm window is
[ADR 0014](../../adr/0014-confirm-window-provisional-commit.md). The decisions
below are the ones the ADRs do not fully carry.

**Membership authority (Q1).** A dedicated static roster key
`bridge.cluster.members`, plus a per-process `member_id` injected by the
composition root (`bridge.WithClusterRollout.MemberID`). The roster is NOT
`cluster.endpoints`: that key is this instance's *capability* map (`{http: …}`),
so a cohort keyed off it would freeze the epoch `["http"]` — a one-member
barrier, i.e. no barrier at all. The member id cannot be derived from
`bridge.instance_id` (empty in a shared-config cohort so each task derives a
unique runtime metric identity), so "this node is in its own roster" is admitted
at startup, not by the blueprint validator.

**Candidate transport (option b).** Each member's OWN config watcher delivers
the candidate bytes; the rollout row carries only digest+version as the
cross-member agreement check, and a booting member stages the document it booted
so it can vote on a rollout that later carries it. Option (a) — pushing bytes
through the row — was rejected as unnecessary machinery since every member
already receives the change. Digest determinism holds because GoBridge performs
NO per-node interpolation on the config load path (a `shared.Secret` is a
literal carried in the document), so two members canonicalise the same document
identically.

**Durable last-committed artifact.** Option (b) alone left a member with no
durable answer to "what should I be running?" independent of the rollout row.
Closed with a second coordination-store row (`ROLLOUT#committed`,
`ports.ClusterCommittedConfigStore`) holding the round-trippable committed
config BYTES + canonical digest (a `ClusterRolloutConfig.Encode`/`Decode` codec
is injected because `bridge` may not import `config/parser`). The adopting
member writes it (idempotent, monotonic); a joiner then **boots on the committed
config** when `current` holds a candidate the barrier has not committed, and the
applier **reconciles** to it when the active row moved on before a member
observed the commit. It advances on Commit (base) and, under a window, only on
**Confirm** (§8.1) — so a crash reboots onto the last *confirmed* generation.
Scoped, fail-safe limitation: no baseline auto-seed (a candidate sitting in
`current` during the write→propose window is indistinguishable from a deploy
baseline, so the artifact is established by the first real commit, not seeded).

**Composition obligations.** A coordinated root MUST wire `config.Validate`
(an Ack proves the candidate passes the `BlueprintValidator` and builds, but not
the runtime route-graph validation that runs at commit — without it a dangling
reference is acked by all, committed, then fails every swap; G2 still holds).
It MUST also re-sync the config manager after a barrier swap
(`Manager.AdoptRunning` / `NotifyApplyResult`), or `ReconfigurePending` /
deep-health `Degraded` can latch despite correct convergence.

**F5b residual — accepted.** No coordinator-claim write before the first
decision: every zombie outcome is fail-safe (a zombie Commit still needs the
full ack barrier; a zombie Abort keeps the old config serving), and the
coordinator renews its lease BEFORE every observation, so a deposed one steps
down without deciding.

**Q3 audit trail — overwrite, retaining the last.** One rollout row (active or
last-decided), readable via `Current`, `/deephealth`, and metrics. A ledger of
N past rollouts is a store-schema change with no consumer yet.

**Confirm window (§8.1).** Two terminal states `RolloutConfirmed`/
`RolloutReverted`; a windowed `Committed` is non-terminal until a fenced
`Confirm` (whole epoch converged) or `Revert` decides it. The coordinator checks
the deadline BEFORE the confirm barrier, so a late-resuming coordinator reverts
rather than flapping a cohort that already began to revert. The revert target is
the cached pre-swap config (generation N−1), captured at the first provisional
observation — always available, even for the first windowed rollout, which has
no prior committed artifact.

**Test coverage** matches §10. Known harness limitation: the confirm-window
**deadman arm** (one member never converges → whole cohort reverts) is proven
deterministically in-process
(`TestClusterRolloutDriver_ConfirmWindow_DeadmanRevert`, real coordinator +
applier + store, injectable convergence). It is NOT reproduced multi-process —
forcing one real member to swap-but-never-converge needs a controllable-
readiness transport the `nodeProcess` harness lacks (the same limitation this
design records for UC-CR6). When that transport lands, UC-CR9 gains the revert
arm across real processes.

## 12. ADRs

The design decisions ship as ADRs:
[0013 — Coordinated cluster config rollout](../../adr/0013-coordinated-cluster-config-rollout.md)
(supersedes 0012 for live-safe deltas) and
[0014 — Confirm window: provisional commit with deadman revert](../../adr/0014-confirm-window-provisional-commit.md)
(extends 0013). Those are authoritative for shipped behavior; this section
replaces the earlier draft ADR that was promoted verbatim at ship time.

## 13. Open questions — resolved, retained for provenance

| Q | Question | State |
|---|---|---|
| Q1 | Membership authority | **Decided:** a dedicated static roster key `bridge.cluster.members`, plus an injected `MemberID` (§11). The earlier answer — the keys of `cluster.endpoints` — was wrong (that key is this instance's capability map). Revisit when endpoint auto-discovery becomes a supported shape; gossip views would stay advisory either way — the frozen list in the rollout row is the safety input |
| Q2 | Rollout deadline default | **Measured:** N=3 propose→all-committed staging ≈ 1 s on DynamoDB Local; `defaultRolloutTTL` = 5 min retained with ample margin (NETCONF's confirmed-commit default is 600 s) |
| Q3 | Retain `Aborted` rollouts as an audit trail, or overwrite? | **Decided:** overwrite, retaining the last. One row holds the active-or-last-decided rollout, exposed via `Current`, `/deephealth`, and the resolution counter. A ledger of N past rollouts is a store-schema change with no consumer yet |
| Q4 | Strict all-ack (this design) vs Kafka-style "commit centrally, fence non-converged members out of serving" | **Deferred.** Strict-abort/revert is simpler and matches the operator contract for cohorts ≤ ~10; the confirm window (§8.1) is the first thing that makes converged-vs-acked distinguishable. Revisit if cohorts grow beyond ~10 |
| F5b | Fence the *first* coordinator decision (claim write), or accept the residual? | **Decided:** accept the residual (§11). Every zombie outcome is fail-safe, and the coordinator renews its lease before each observation |
