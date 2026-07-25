# Design: Coordinated cluster config rollout (barrier protocol)

Status: **partially implemented** — Phases 1–4 done (protocol state, durable
store, orchestration logic + guard-lift, and the full barrier drive + operator
surface); Phases 5–7 not started (§11). The barrier is NOT wired into any
composition root, so shipped behavior is unchanged: a clustered deployment still
refuses live reloads per ADR 0012. At ship time §12 is promoted to
`docs/adr/0013-…` (ADRs here document shipped behavior only) and 0012 gains
`Superseded by 0013`.
Date: 2026-07-25 · Relates to: ADR 0012, `PROD_READY_ISSUES.md` §3 CLUSTER-3,
`docs/runbooks/cluster-config-rollout.md` · Prior art & split-brain analysis:
`cluster-config-rollout-research.md` (xDS, ZooKeeper reconfig, Raft, KRaft, K8s,
NETCONF confirmed-commit, NSO, Paxos Commit, fencing theory — cited below)

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
rollout at a time; generation is monotonic.

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
  cross-member agreement check (§11 Phase 4 records why, and the joiner rule
  that makes it safe).

Adapters (Layer 3): `adapter_store_dynamodb_rollout` (DynamoDB, conditional
writes — same idioms as `dynamodblease`), `adapter_store_memory_rollout`
(tests / single-binary). Both register per `PLUGIN.md`; both must pass the
new `ports/storetest.RunClusterRolloutStoreTests` conformance suite.

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
only when `cluster.rollout: coordinated` is configured **and** the delta is
live-safe (§8); everything else still refuses fail-closed exactly as today.

## 7. Failure matrix

| # | Failure | Outcome | Mechanism |
|---|---|---|---|
| F1 | Member crashes before Ack | Rollout aborts at deadline; survivors keep old config; member rejoins on old (still-committed) gen | Deadline + joiner rule |
| F2 | Member Nacks (validation/build fails) | Abort everywhere; candidates discarded | Coordinator on first Nack |
| F3 | Coordinator crashes mid-rollout | Rollout lease expires (TTL); successor resumes from store state and commits/aborts | LeaseStore election + idempotent coordinator |
| F4 | Two concurrent proposes | Second `Propose` fails the conditional create (I1) | CAS |
| F5 | Deposed coordinator tries Commit/Abort **after the live one decided** | Rejected — stale fencing token (I3) | Token condition |
| F5b | Deposed coordinator decides **first** (no decision recorded yet) | **Not fenced** — accepted. Fail-safe: a zombie Commit still needs the full ack barrier (I2), a zombie Abort just keeps the old config serving. Bounded in practice by the successor's lock-delay (§6) | Residual — closing it needs an explicit coordinator-claim write before the first decision (see Phase 4) |
| F6 | Membership changes mid-rollout (join/leave) | Abort; operator retries (cheap — nothing swapped) | Strict epoch equality; simplest safe rule |
| F7 | Member crashes after Commit, before its swap | Rejoins and boots the committed gen — same config, no split | Joiner rule |
| F8 | Committed config fails to converge on a node (e.g. broker unreachable) | No distributed rollback; that node latches `ConfigDegraded` via MQTT-R1, alarmed — parity with single-node behavior | §2 N5 |
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
wires no rollout store makes every coordinated reload fail closed.

### 8.1 Confirm-window extension (Model B — opt-in, later phase)

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
trade, accepted when a bad config staying active is worse.

## 9. Observability

- Metrics: `ClusterRolloutState` gauge (per state), `ClusterRolloutAcks`
  gauge (`acks/epoch`), `ClusterRolloutResolved` counter
  (`outcome=committed|aborted`, `reason=…`).
- Deep health: a `rollout` section (generation, state, acked members).
- Runbook: `cluster-config-rollout.md` gains a "coordinated mode" chapter;
  the whole-cohort procedure stays as the replacement-required path.

## 10. Test plan (all gates deterministic — TESTS.md rules)

Unit (`domain/persistence`, `bridge/`):
- Rollout state machine: table-driven transitions + every invariant I1–I5.
- Rollout-class decision: table of deltas → live-safe/replacement-required.
- Coordinator logic on `clocktest` (deadline abort, resume-from-store,
  stale-token rejection) with fake stores.

Conformance (`ports/storetest`):
- `RunClusterRolloutStoreTests`: CAS races (double propose, commit vs abort
  race, stale fencing token, ack-after-abort), run against memory + DynamoDB
  implementations (mandatory per TESTS.md §3.3).

Integration (`tests/integration/`, ddblocal + mqttlocal):
- Single-process happy path: propose → ack → commit → swap through the real
  Supervisor (extends the existing
  `TestSupervisorMQTTReload_ConfigDrivenBrokerTruth` harness).
- Abort path: Nack member (invalid plugin) → nothing swaps, old config
  serves, `ClusterRolloutResolved{outcome=aborted}` emitted.

Long-running (`tests/longrunning/`, `nodeProcess` multi-process harness —
real processes, real DynamoDB, real broker; barriers are stdout tokens +
store rows, never sleeps):
- UC-CR1: N=3 happy path — all nodes swap; equal `config_version`; traffic
  proof before and after.
- UC-CR2: SIGKILL the coordinator mid-staging — successor (lease TTL) resumes
  and commits; total time bounded by lease TTL + deadline.
- UC-CR3: SIGKILL one member before Ack — deadline abort; survivors
  unchanged; killed member rejoins on the old committed gen.
- UC-CR4: one member Nacks — abort propagates; no node swaps.
- UC-CR5: replacement-required delta — refused at preflight with the class
  reason; rollout aborts; runbook pointer in the reason.
- UC-CR6: committed config with unreachable broker — every node marks
  MQTT-R1 `ConfigDegraded` (no rollback), mirroring the single-node reload
  test.

## 11. Phasing

Each phase is a separately mergeable slice, in dependency order — a phase only
consumes what earlier phases shipped, and each lands green on `make lint` +
`make test`. The §-references map a phase to the part of this design it
implements.

### Phase 1 — Protocol state (§4, §5 port) — ✅ IMPLEMENTED

- `domain/persistence`: `Rollout`, `RolloutAck`, `RolloutProposal`, state
  machine + invariants I1–I5.
- `ports`: `ClusterRolloutStore`; `adapter_store_memory_rollout`.
- Done: `ports/storetest.RunClusterRolloutStoreTests` (CAS races, stale
  token, ack-after-abort) green on the memory adapter.

### Phase 2 — Durable store (§5 store invariants) — ✅ IMPLEMENTED

- `adapter_store_dynamodb_rollout` — delivered as
  `adapters/aws/store/dynamodbrollout`: conditional writes + `ConsistentRead`,
  `dynamodblease` idioms (including its error-classification policy: ctx
  sentinels pass through, throttling is `ErrThrottled`), single-row + monotonic
  `rev` optimistic-lock CAS, `attribute_not_exists(#pk)` for the fresh-propose
  single-winner, single-Region invariant stated in code docs. Domain gained a
  reconstitution factory (`persistence.RolloutSnapshot` / `Snapshot` /
  `RehydrateRollout`) so the aggregate — not a parallel state machine — owns
  every invariant across a serialize/reload round trip.
- `.go-arch-lint.yml` component + `lint-arch-mapping-test.sh` sentinel.
- Done: the same conformance suite green against DynamoDB Local (25/25,
  including the concurrency races), race-clean, plus unit round-trip/decode and
  corruption fail-closed (unit + ddblocal) coverage.

### Phase 3 — Orchestration logic + guard-lift (§6, §7, §8 classes) — ✅ IMPLEMENTED

Scope complete; the barrier-drive items it deferred moved to Phase 4.
Delivered (all in `bridge/`, unit-tested on fakes + `clocktest`):

- **Rollout classes (§8):** `classifyRolloutDelta` (live-safe vs
  replacement-required) reuses the exact per-node reload preflights
  (`durableSessionIdentityChanged`, `storeIdentityChanged`,
  `leaseSessionIDChanged`, deployment-mode change, `destructiveReloadShape`),
  so the coordinated path can never admit a delta the single-node path rejects.
- **Config surface + opt-in:** `ports.ClusterConfig.Rollout` field;
  `coordinatedRollout(cfg)` predicate (clustered ∧ `rollout: coordinated`).
- **Guard lift (§6):** `classifyClusterReload` (proceed / refuse / coordinated)
  wired into `Supervisor.apply`, default fail-closed. Phase 3 recognised a
  coordinated + live-safe delta as eligible but, with the drive unwired, failed
  it closed with a coordinated-specific error rather than a success-shaped
  deferral that would silently drop the change. Phase 4 drives it instead.
- **Coordinator decision core:** `decideRollout` (F1 deadline, F2 nack, F6
  membership-change; commit-before-deadline, membership-before-commit ordering)
  + `coordinatorStep` (fenced observe→act: F3 resume, F5 stale-token, F9 outage)
  + `firstSideEffectAllowed` lock-delay gate + `rolloutCoordinator.elect/observe`
  (caller-driven, so lock-delay is a pure predicate with no goroutine in tests).
- **Applier units:** `candidateConfigDigest`/`verifyCandidateDigest` (F10),
  `nodeRolloutGate` high-water (F7), `evaluateProposal` (digest-first → class).
- **Failure matrix:** F1,F2,F3,F6,F7,F9,F10 covered by units; F4 + F5 by the
  Phase-1 conformance suite; F8 by the reused single-node post-swap convergence
  watch (`watchPostSwapConvergence` → `ConfigDegraded`, §2 N5).

Deferred to Phase 4: the coordinator `Run` loop, the applier goroutine, gate
persistence, and the real membership source.

### Phase 4 — Operator surface (§6 proposer, §8 config key, §9) — ✅ IMPLEMENTED

The barrier now drives end to end: a live-safe delta in a coordinated cohort is
proposed, every member votes, a fenced coordinator decides, and members swap only
on `Committed`. Delivered:

- **Proposer (§6)** — `rollout_barrier.go`: `ClusterRolloutConfig` +
  `WithClusterRollout`, `propose`, candidate staging, and `joinActive` (an
  `ErrAlreadyExists` is success ONLY when `Current` confirms the active rollout
  carries this node's exact digest and an epoch containing it). **Guard lift**,
  extracted to `rollout_guard.go` (the housekeeping item): propose-and-defer,
  failing CLOSED wherever the delta could not be proposed — an unproposed delta
  is never reported as deferred, since an in-band applier resolves a deferral as
  committed.
- **Applier (§6)** — `rollout_applier.go`: observe → `evaluateProposal` →
  build-without-swap via `Builder.Plan` → `Ack`/`Nack`; on `Committed` the local
  swap runs through the normal apply path (`applyBarrierCommitted`), keeping the
  hardened recover/converge behaviour (F8) instead of a parallel one. **Drive**
  (`rollout_loop.go`): one goroutine per node ticking the applier and, for the
  lease holder, the coordinator (`elect` → lock-delay → renew → fenced
  `observe`), releasing the lease on orderly stop. `Run` stops the drive BEFORE
  draining, so a committed swap cannot publish a runtime after shutdown.
- **Startup gate** (`rollout_joiner.go`) — this node must be in its own roster
  and must not boot onto a config the barrier has not committed. **Admission**:
  `config/validate.go` rejects an unknown `rollout` value, `coordinated` without
  a cohort, an empty roster, and duplicate/blank member ids.
- **Observability (§9)** — `ClusterRolloutState` (one-hot gauge per state),
  `ClusterRolloutAcks`/`Epoch`, `ClusterRolloutResolved` (counted once per
  generation), and a `/deephealth` `rollout` section naming the epoch, who voted,
  whether THIS member staged the candidate, and whether it actually applied it.
- **Tests** — unit coverage of both halves, whole-barrier tests where one process
  runs both, resilience tests for the transient-failure classifications, and an
  integration suite driving happy path, nack→abort, and replacement-required
  refusal against real DynamoDB.

#### Decisions this phase had to make

- **Q1 RE-DECIDED — membership authority.** The original answer (keys of static
  `bridge.cluster.endpoints`) was **wrong**: that key is THIS instance's
  CAPABILITY map (`{http: …}`, validated to carry an `http` key), not a peer
  roster, so every cohort would have frozen the epoch `["http"]` — a one-member
  barrier committing on a single ack, i.e. no barrier at all. The roster is now
  its own key, **`bridge.cluster.members`**; the member id is injected via
  `WithClusterRollout.MemberID` because a cohort shares one config document
  (`bridge.instance_id` is empty there so each task derives a unique runtime
  identity). "This node is in its own roster" is therefore admitted at startup,
  not by the blueprint validator.
- **Candidate transport (§5) — option (b) for Phase 4, with a NAMED residual.**
  Each member's OWN config watcher delivers the candidate; the rollout row's
  digest is the cross-member agreement check; the proposer stages what it
  proposed, and a booting member stages the document it booted so it can vote on
  a rollout that later carries it. Option (a) was rejected as unnecessary
  machinery given every member already receives the change. There are no
  *fetched* candidate bytes, so "verify the digest before decoding hostile input"
  is satisfied by construction. **But (b) is not fully closed** — see the
  residual below. It is why the barrier stays unwired.
- **Digest determinism — RESOLVED (was "UNPROVEN").** The digest covers
  **revealed** secrets, which is safe because GoBridge performs NO per-node
  interpolation on the config load path (a `shared.Secret` is a literal carried
  in the document), so two members loading the same document canonicalise
  identically. Pinned by a unit test; UC-CR7 is the multi-process proof.
- **`nodeRolloutGate` persistence — NOT NEEDED.** The store admits one active
  rollout and `Current` returns only the newest generation, so a stale generation
  has no channel to a restarted node; re-adoption is caught by CONTENT, not by a
  counter. A per-node durable write path would add failure modes and remove none.
- **F5b — residual ACCEPTED, explicitly.** No coordinator-claim write: every
  zombie outcome is fail-safe (a zombie Commit still needs the full ack barrier;
  a zombie Abort keeps the old config serving), and `tick` renews the lease
  BEFORE every observation, so a deposed coordinator steps down without deciding.
- **Q3 — audit trail: overwrite, retaining the last.** One rollout row (active or
  last-decided), readable via `Current`, `/deephealth`, and metrics. A ledger of
  N past rollouts is a store-schema change with no consumer yet.
- **Transient failures must not become permanent cohort outcomes.** A vote is
  unretryable (I5) and one Nack aborts (F2), so a build failing on shutdown, a
  throttled store, or an unavailable dependency ABSTAINS. Likewise a coordinator
  does not step down on a transient renewal error (that costs a fresh lock
  delay), and a failed post-commit swap retries boundedly before reporting.

#### BLOCKING residual — no last-committed artifact in the config source

An adversarial review traced three reachable sequences that reduce to one missing
fact. Under (b) the operator's change is durably in the config source **before**
the barrier decides, and the source keeps only a `current` slot — so a member has
no durable answer to "what should I be running?" independent of the rollout row:

1. **Restart into the write→propose window.** The source holds the new document
   and no rollout carries it yet, so the joiner rule (which refuses only what the
   barrier explicitly did not commit) lets the member boot on it — alone.
2. **A commit overwritten before every member observed it.** `Propose` replaces
   the terminal row, so a member that had not yet ticked never sees the
   generation it should have applied.
3. **Restart after an abort.** The source still holds the rejected document, so
   the joiner rule refuses the boot — correct and fail-closed, but it means the
   cohort cannot replace an instance until an operator rolls the source back.
   F1's "member rejoins on the old (still-committed) gen" is therefore **not
   achievable** under (b) as written.

Boot-staging removes the *permanent* deadlock in (1) and (2) — the member can now
vote on the next rollout and converge — but a bounded mixed-version window
remains, and (3) is a real availability cost. **Phase 5 must close this before
the barrier is wired anywhere**, by giving the cohort a durable last-committed
configuration artifact (option (a)'s staged/committed pair, or an equivalent).

Second, smaller limitation: an Ack proves the candidate passes the wired
`BlueprintValidator` and that this member can build its stores and runtime
options; it does NOT cover the runtime's route-graph validation, which runs in
the COMMIT phase. A coordinated deployment **must** therefore wire
`config.Validate` — without one `Builder.Plan` trusts the input, so a dangling
reference is acked by every member, committed, and then fails every member's swap
(G2 still holds, since all members fail identically and recover the old config,
but the change costs a cohort-wide failed swap rather than an abort).

`WithClusterRollout` is wired by no composition root, so shipped behavior is
unchanged — the file-based AWS root refuses clustered live reload per ADR 0012.
Wiring it is Phase 6's ship step, gated on Phase 5.

### Phase 5 — Cluster proof (§10 long-running)

- UC-CR1…UC-CR6 on the `nodeProcess` harness (real processes, real
  DynamoDB, real broker).
- **UC-CR7 — cross-member digest agreement.** Three real processes, each
  loading the change through its OWN config source, must compute the SAME
  candidate digest and all Ack. This is the Phase-4 determinism item's proof;
  without it a cohort-wide Nack storm only shows up in production.
- **UC-CR8 — foreign-rollout collision.** A second, different change proposed
  while one is in flight must be refused at the proposer with the running config
  still serving, and must NOT be reported as a deferral (`joinActive`).
- Q2 sizing: measure staging duration on the N=3 fleet and set the rollout TTL
  default from it (today `defaultRolloutTTL` = 5 min, unvalidated).
- Done: suite green; every barrier is a store row or stdout token, no
  sleeps.

### Phase 6 — Ship (§12)

- Runbook "coordinated mode" chapter; promote §12 verbatim to
  `docs/adr/0013`; flip CLUSTER-3 status in `PROD_READY_ISSUES.md`.
- The runbook must carry the operator-facing consequences of the §8
  replacement-required classes — notably that changing
  `bridge.cluster.endpoints` or `bridge.cluster.rollout` keeps ADR 0012's
  whole-cohort procedure even in a coordinated cohort.

### Phase 7 — Confirm window (§8.1; optional, separate go/no-go)

- `confirm_window` config, provisional swap + deadman revert to N−1,
  `Converged`/`Confirmed` records.
- Q4 (strict all-ack vs Kafka-style "commit centrally, fence non-converged
  members out of serving") is revisited here, not earlier: the confirm window
  is the first mechanism that makes converged-vs-acked distinguishable.
- Done: UC-CR9 — one node never converges → whole cohort deadman-reverts;
  traffic proof on the reverted generation.

## 12. Draft ADR 0013 (promote verbatim at ship time)

> # 0013 — Coordinated cluster config rollout for live-safe deltas
>
> Status: accepted · Supersedes: 0012 (for live-safe deltas only)
>
> ## Context
> ADR 0012 refuses every non-no-op clustered live reload because the cluster
> had no version barrier, readiness gate, or coordinated rollback. The
> refusal was safe but forces orchestration downtime for every change,
> including trivially safe ones.
>
> ## Decision
> GoBridge implements a staged, all-member barrier protocol over the shared
> store: an operator posts a change to any node; it is proposed as a
> candidate generation with a frozen membership epoch; every member
> validates and builds it, then acks; a lease-elected, fencing-protected
> coordinator commits only when acks cover the epoch, else aborts (timeout,
> Nack, membership change). Members swap only on observing Committed.
> Post-commit convergence remains per-node (MQTT-R1). The protocol is
> opt-in (`cluster.rollout: coordinated`), requires the versioned DDB config
> source, and applies only to deltas passing the live-safe preflight —
> durable-identity and store-target changes keep 0012's whole-cohort
> replacement.
>
> ## Consequences
> - No supported path can produce a mixed-version cohort: members swap only
>   after a store-atomic Commit that required every epoch member's ack.
> - Coordinator and member crashes resolve via lease TTL + rollout deadline;
>   all protocol state is durable, so recovery is resumption, not repair.
> - Commit ≠ converged: a committed config can still degrade per node and is
>   alarmed, not rolled back — unchanged from single-node semantics.
> - EFS/file-sourced clusters and destructive deltas keep ADR 0012.
>
> ## Rejected alternatives
> - Node-to-node RPC coordination — new failure surface; the store bus
>   already carries leases/outbox safely.
> - External consensus (raft/etcd) — operational dependency out of
>   proportion to one barrier.
> - Rolling per-node apply with eventual convergence — unbounded
>   mixed-version window (0012's original rejection stands).
> - Distributed post-commit rollback — requires the same barrier again with
>   traffic in flight; alarm-and-operate matches the existing model.

## 13. Open questions — each owned by a phase

| Q | Question | Owned by | State |
|---|---|---|---|
| Q1 | Membership authority | Phase 4 | **Decided (revised):** a dedicated static roster key `bridge.cluster.members`, plus an injected `MemberID`. The earlier answer — the keys of `cluster.endpoints` — was wrong: that key is this instance's CAPABILITY map, so the epoch would have been `["http"]`, a one-member barrier. No heartbeat subsystem. Revisit when endpoint auto-discovery becomes a supported shape; gossip views would stay advisory either way — the frozen list in the rollout row is the safety input |
| Q2 | Rollout deadline default (`2 × convergence budget floor + member build budget` ≈ 5 min; NETCONF's confirmed-commit default is 600 s) | Phase 5 | Open — needs sizing against a real fleet, so it is measured on the UC-CR1 run |
| Q3 | Retain `Aborted` rollouts as an audit trail, or overwrite on the next Propose? | Phase 4 | **Decided:** overwrite, retaining the last. One row holds the active-or-last-decided rollout; its state, reason, and vote sets are exposed via `Current`, `/deephealth`, and the resolution counter. A ledger of N past rollouts is a store-schema change with no consumer yet |
| Q4 | Strict all-ack (this design) vs Kafka-style "commit centrally, fence non-converged members out of serving" — our lease failover already *is* a fencing mechanism | Phase 7 | Deferred — strict-abort is simpler and matches the operator contract; the confirm window is the first thing that makes converged-vs-acked distinguishable. Revisit if cohorts grow beyond ~10 |
| F5b | Fence the *first* coordinator decision (claim write), or accept the residual? | Phase 4 | **Decided:** accept the residual. Every zombie outcome is fail-safe, and the coordinator renews its lease BEFORE each observation, so a deposed one steps down without deciding. Revisit with the confirm window (§8.1) |
