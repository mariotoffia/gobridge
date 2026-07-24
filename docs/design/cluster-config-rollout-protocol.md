# Design: Coordinated cluster config rollout (barrier protocol)

Status: **proposed design — not implemented**. When shipped, the draft ADR in
§12 is promoted to `docs/adr/0013-…` (ADRs in this repo document shipped
behavior only) and ADR 0012 gains a `Superseded by 0013` line.
Date: 2026-07-23
Relates to: ADR 0012, `PROD_READY_ISSUES.md` §3 CLUSTER-3,
`docs/runbooks/cluster-config-rollout.md`
Prior art & split-brain analysis: `cluster-config-rollout-research.md`
(xDS, ZooKeeper reconfig, Raft, KRaft, K8s, NETCONF confirmed-commit, NSO,
Paxos Commit, fencing theory — the design choices below cite its rules)

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
  token (a deposed coordinator cannot flip state).
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
- **Membership** — the epoch snapshot at Propose comes from the validated
  `cluster.endpoints` map (static) or, when endpoint auto-discovery is in
  use, the same registry the lease `endpoints` maps already publish. The
  design deliberately freezes membership at Propose (§7 F6).
- **Config transport** — the DDB config source rows (existing CAS loader)
  carry the candidate payload; the rollout row stores only digest+version.
  Members fetch the candidate bytes from the config source and verify the
  digest.

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
| F5 | Deposed coordinator tries Commit/Abort | Rejected — stale fencing token (I3) | Token condition |
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

New config key (validated in `config/validate.go`):

<!-- docs-example: skip -->
```yaml
bridge:
  deployment_mode: clustered
  cluster:
    rollout: refuse | coordinated   # default: refuse (today's behavior)
```

`coordinated` additionally requires the DDB config source and a configured
rollout store; the validator rejects `coordinated` + file/EFS source.

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

Each phase is a separately mergeable slice, in dependency order — a phase
only consumes what earlier phases shipped, and each lands green on
`make lint` + `make test`. The §-references map the phase to the part of
this design it implements.

### Phase 1 — Protocol state (§4, §5 port)

- `domain/persistence`: `Rollout`, `RolloutAck`, `RolloutProposal`, state
  machine + invariants I1–I5.
- `ports`: `ClusterRolloutStore`; `adapter_store_memory_rollout`.
- Done: `ports/storetest.RunClusterRolloutStoreTests` (CAS races, stale
  token, ack-after-abort) green on the memory adapter.

### Phase 2 — Durable store (§5 store invariants)

- `adapter_store_dynamodb_rollout` — conditional writes + `ConsistentRead`,
  idioms from `dynamodblease`; single-Region invariant stated in code docs.
- `.go-arch-lint.yml` component + `lint-arch-mapping-test.sh` sentinel.
- Done: the same conformance suite green against DynamoDB Local.

### Phase 3 — Orchestration (§6, §7, §8 classes)

- Supervisor applier: preflight → build-without-swap → Ack/Nack → swap on
  `Committed`; node-side `(epoch, generation)` token check.
- Coordinator: lease-elected, lock-delay, commit/abort. Guard lift behind
  `cluster.rollout: coordinated`, live-safe deltas only.
- Done: unit coverage for every failure row F1–F10 on fakes + `clocktest`.

### Phase 4 — Operator surface (§6 proposer, §8 config key, §9)

- Admin config-txn API propose path; `config/validate.go` rules
  (`coordinated` requires DDB source + rollout store); metrics + deep-health
  `rollout` section.
- Done: single-process integration tests (§10) — happy path + Nack abort.

### Phase 5 — Cluster proof (§10 long-running)

- UC-CR1…UC-CR6 on the `nodeProcess` harness (real processes, real
  DynamoDB, real broker).
- Done: suite green; every barrier is a store row or stdout token, no
  sleeps.

### Phase 6 — Ship (§12)

- Runbook "coordinated mode" chapter; promote §12 verbatim to
  `docs/adr/0013`; flip CLUSTER-3 status in `PROD_READY_ISSUES.md`.

### Phase 7 — Confirm window (§8.1; optional, separate go/no-go)

- `confirm_window` config, provisional swap + deadman revert to N−1,
  `Converged`/`Confirmed` records.
- Done: UC-CR7 — one node never converges → whole cohort deadman-reverts;
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

## 13. Open questions (decide before phase 3)

- Q1 Membership authority when `cluster.endpoints` is absent: dedicated
  heartbeat rows in the rollout store vs deriving from lease endpoints.
  Leaning: heartbeat rows (explicit epoch, no coupling to session leases).
  Research note: gossip-style views are advisory only — the frozen list in
  the rollout record is the safety input either way.
- Q2 Rollout deadline default: proposal is `2 × convergence budget floor +
  member build budget` ≈ 5 minutes (NETCONF's confirmed-commit default is
  600 s — same order of magnitude); needs sizing against slow fleets.
- Q3 Should `Aborted` rollouts be retained (audit trail) or overwritten by
  the next Propose? Leaning: retain last N (IOS-XR keeps 100 commit IDs;
  a generation ledger, not a boolean), expose via admin API.
- Q4 Long-term evolution: strict all-ack (this design) vs Kafka-style
  "commit centrally, fence non-converged members out of serving" — our
  lease failover already *is* the fencing mechanism (a member that cannot
  apply could release leases and a converged member takes over). Deferred:
  strict-abort is simpler and matches the operator contract; revisit if
  cohort sizes grow beyond ~10.
