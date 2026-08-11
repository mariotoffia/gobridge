# Research: cluster config rollout — prior art and split-brain analysis

Status: research record backing `cluster-config-rollout-protocol.md`.
Date: 2026-07-23. Method: primary sources (RFCs, papers, vendor docs) — full
citations in §7.

Question: how do production systems propagate a config change to a cluster
and apply it all-or-nothing with per-node acknowledgement; can split-brain
occur in a cluster that coordinates through one linearizable store; and what
should GoBridge reuse vs build?

---

## 1. Prior art digest

| System | Shape | On one node failing | Rollback |
|---|---|---|---|
| **Envoy xDS** | control plane pushes versioned config; each client validates and ACKs (echo version) or NACKs (previous version + error_detail) | NACKer keeps **last-good config** and keeps serving; fleet deliberately mixed-version; control plane tracks per-client SYNCED/ERROR/STALE (CSDS) | none — fix forward |
| **ZooKeeper dyn-reconfig** (Shraer ATC'12) | reconfig is an ordinary op in the replicated log; **precondition**: quorum of the *new* config connected + caught up before proposing; commit in old config, activate only after new config has full history | refuses up front (`NewConfigNoQuorum`); an uncommitted reconfig vanishes like any failed op | none needed — commit-or-vanish |
| **Raft membership** (Ongaro §4) | config lives in the log; change by at most one server at a time (quorum overlap); newcomers catch up as **non-voters** first; abort if they can't | leader **aborts** the change ("drowned sailors" lesson); uncommitted config entry truncated → fall back to prior entry | fall back to prior log entry |
| **Kafka KRaft** | all config = records in a Raft-replicated metadata log; brokers are observers that replay it; heartbeats report applied offset | change commits centrally regardless; lagging broker is **fenced out of serving** (loses leaderships, hidden from clients), never blocks the change | none — append newer record |
| **Kubernetes / Argo** | declarative rolling update; bounded unavailability (`maxUnavailable`); pod counts only after readiness + `minReadySeconds` | rollout **stalls** at the availability floor; old pods keep serving; `ProgressDeadlineExceeded` is a *condition*, not an action; Argo adds metric-driven auto-abort **as an opt-in layer** | old ReplicaSet retained; `rollout undo` = re-activate |
| **etcd / Consul watches** | write versioned value; consumers watch. Watches are **hints, not truth** (etcd watches not linearizable; Consul wakes spuriously) | none built in — per-node "applied version X" ack records must be built on top | write old value as new revision |
| **NETCONF :confirmed-commit** (RFC 6241 §8.4) | edit candidate → validate → `commit confirmed` starts a deadman timer (default 600 s) → plain commit confirms; timer expiry / session death / reboot ⇒ **MUST revert** to pre-commit snapshot; `persist` token lets another session confirm | n/a (single device) — the pattern generalizes below | revert = load + commit the previous config (normal engine) |
| **JunOS / IOS-XR commit confirmed** | same, operator-facing ("don't lock yourself out"); XR: every commit gets a **commit ID**, rollback *creates a new commit ID* | XR: exiting the session with a pending confirm ⇒ immediate rollback | rollback = new forward commit of old content |
| **Cisco NSO network-wide txn** | one transaction across N devices: validate → prepare (stage on candidate where possible) → commit; capable devices sit in **per-device confirmed-commit windows**; global commit = confirm all | **the crux**: any failure ⇒ *don't confirm the rest* — their deadman timers revert them. **Abort is the default outcome, achieved by inaction.** Non-capable devices get compensation (stored reverse diff); failed compensation ⇒ honest `out-of-sync` flag + alarm + manual resync | stored reverse transaction applied as a **regular transaction** |
| **Feature flags / expand-contract** | trial window + automated health verdict + promote-or-revert; revert cheap because both versions stay alive simultaneously | auto-rollback on metric regression (LaunchDarkly guarded rollouts) | pointer flip |

RFC 8342 (NMDA) formalizes the last gap: `intended` vs `operational`
datastores — *accepted ≠ applied*; convergence is checked by diffing them,
tolerating transient remnants. (GoBridge's convergence watch is
exactly this instrument.)

## 2. The transferable rules (cross-system synthesis)

1. **Nobody swaps all nodes atomically.** Every system does: atomic
   *decision* in one place (log/quorum/store) + asynchronous per-node
   convergence. (Ongaro: "it isn't possible to atomically switch all of the
   servers at once.")
2. **Last-good config on rejection** — a bad config must never take a
   healthy node out (xDS NACK, K8s unready pods, Raft truncation-fallback).
3. **Version/epoch on everything; every ACK names the version it acks.**
4. **One change in flight, ever** (ZK `ReconfigInProgress`, Raft leader
   rejects concurrent changes). Serialization via CAS is what keeps the
   reasoning tractable.
5. **Precondition the cohort before committing** (ZK: new quorum connected +
   caught up; Raft: catch up as non-voter, abort if it can't).
6. **Per-node ACK is a durable store record / heartbeat field**, never
   coordinator memory (Kafka applied-offset, CSDS states).
7. **Nobody blocks a change on a dead node forever**: fence laggards out of
   serving (Kafka) or stall visibly at a bounded floor (K8s). A deadline
   with abort is the minimum viable form of this.
8. **Auto-rollback is an opt-in layer above the base protocol** (Argo over
   K8s; confirmed-commit over plain commit), never the base itself.
9. **Rollback is always roll-forward**: apply the previous config through
   the *same* pipeline as any change (JunOS loads+commits previous; XR
   rollback creates a new commit ID; NSO applies the stored reverse as a
   regular transaction). Revert then gets tested every time apply is tested.
10. **Provisional apply + deadman timer is how "one of N failed after others
    applied" is solved** (NSO): everyone applies under an auto-revert
    window; commit = confirm everyone; failure = confirm no one — reverts
    happen locally, by inaction, with no coordinator scramble.
11. **Watches are hints; linearizable reads are truth** — notify → re-read
    consistently → compare version → apply → ack.
12. **Hold an exclusive change-lock across a confirm window** — snapshot
    revert wipes concurrent edits (RFC 6241 warning).

## 3. Split-brain analysis for GoBridge

Theory (Kleppmann fencing; Gray & Lamport *Consensus on Transaction
Commit*; AWS Builders' Library leader election; DynamoDB docs):

- **"2PC with the decision recorded in a highly-available linearizable
  store" is Paxos Commit** — 2PC's blocking case (coordinator dies after
  prepare) is engineered out because the commit-point is a durable store
  item any node can read; the client-side coordinator is disposable.
  DynamoDB per-item CAS is internally Paxos-replicated **across AZs** —
  single-Region multi-AZ gives AZ-failure tolerance *and* linearizability.
- **Divergent authoritative histories (the thing that needs "merge") are
  impossible by construction** given three invariants: (1) coordination
  tables are **single-Region** (never MREC global tables — those resolve
  concurrent regional writes last-writer-wins and reintroduce store-level
  split-brain); (2) every authoritative transition is a conditional write
  (one winner; loser gets `ConditionalCheckFailedException`); (3) decisions
  read with `ConsistentRead` (never GSI/Streams/eventual reads). Conflict is
  *rejected before commit*, so a second history never exists — merge
  machinery (Dynamo-style siblings/CRDTs) is what you buy out of by paying
  for coordination. **Answer to "can split-brain happen / how do we merge
  brains?": prevention by construction; merge is never needed.**
- **Residual stale-actor windows** (not split-brain, bounded, and fenced):
  - *Zombie coordinator* after lease loss (GC pause / network delay):
    harmless for every store-side effect because writes carry the fencing
    epoch and fail; bound the non-store window with a stop-work margin on a
    monotonic clock. New coordinator additionally waits out one old-lease
    duration before its first side effect (Chubby lock-delay; DynamoDB does
    the same internally).
  - *Member partitioned from the store while others commit*: it keeps
    serving the old config — but GoBridge **self-fences**: exclusive
    sessions step down after `MaxRenewFails` renewal failures
    (`runtime/session/manager_lease.go`, `errLeaseLostAfterRenewal`) and
    outbox drains require fenced conditional claims (`ports/stores.go`), so
    all *durable/exclusive* work stops on its own. Residual: lease-free
    ephemeral routes continue on stale config for the partition's duration —
    disclosed, alarmed via the member's missing heartbeat.
  - *Store failover*: seconds of write unavailability, never divergence —
    design for stall, not conflict.
- **Membership churn**: gossip/failure detection is advisory; the **frozen
  member list in the rollout record is the safety input**; any change during
  a rollout aborts it (freeze-and-abort — the ZK/Raft "one change at a
  time" lesson without quorum-overlap math).
- **Node-side fencing**: each member persists the highest `(epoch,
  generation)` it applied and rejects lower — a paused-then-resumed
  coordinator's late push is thereby harmless at the *node*, not just at
  the store.

## 4. Model comparison and recommendation

| | Model A — validate-stage → atomic flip (protocol doc v1) | Model B — provisional apply + confirm window (NETCONF/NSO style) | Model C — rolling per-node (K8s style) |
|---|---|---|---|
| What Ack proves | validated + **built** (runtime constructed, not started) | actually **converged** against the real broker | converged, one node at a time |
| Mixed-version serving window | none pre-commit; post-commit only per-node swap skew | the whole confirm window (all nodes on trial gen) | the whole rollout (by design) |
| Service cost on MQTT exclusive sessions | one swap (prepare-commit stops old before new) | **two** disruptions on failure (apply + revert) — the JunOS trade | N× sequential handovers (lease churn) |
| Failure of one node | abort before anyone swapped | don't-confirm ⇒ everyone auto-reverts locally (deadman) | stall at floor, old nodes serve |
| Complexity | lowest (no revert path needed) | medium (timer + revert = re-apply gen N−1 via normal pipeline) | highest for exclusive-identity transports |

Recommendation: **A as the base protocol** (phase 1–5 of the protocol doc),
**B as the opt-in confirm-window layer on top** (rule 8) — commit is followed
by a per-node deadman window of T seconds; nodes report convergence
(NMDA-style intended-vs-operational); the coordinator writes
`Confirmed` when all converge, else stays silent and every node reverts
locally by re-applying generation N−1 **through the normal apply pipeline**
(rule 9 — revert is not a special path). C is rejected for exclusive-identity
MQTT transports (client-ID conflicts forbid keeping both generations
connected; sequential lease handovers multiply disruption).

## 5. Do we need a new interface, or reuse?

Reuse (unchanged): `ports.LeaseStore` (fenced coordinator election),
`ports.EndpointResolver` + `cluster.endpoints` (membership input), the DDB
config source (candidate payload, CAS versions), the admin config-transaction
API (operator front door; its commit response already reports
`rolled_back`-style outcomes), `bridge.Supervisor` prepare/commit (candidate
build), convergence watch (the NMDA "operational" check), lease
step-down + fenced claims (self-fencing).

Build (one small port + one state machine): `ClusterRolloutStore` and the
rollout/coordinator orchestration — per the protocol doc. No external
coordination system (etcd/ZK/Consul) is warranted: prior art's reasons for
those (quorum reads, watches) are already covered by DynamoDB CAS +
consistent reads, and adding a second coordination system contradicts the
deployment posture (KRaft's whole existence is the argument against running
an extra coordination service).

## 6. The operator's 4-step model, mapped

1. *"POST to one node, propagates, each accepts candidate"* → admin txn API
   → `Propose` (CAS) → members fetch + digest-verify + validate + build →
   `Ack`. (Steps: protocol doc §3/§6.)
2. *"Each applies with watchdog timer, one fails ⇒ roll back"* → Model B
   confirm window: provisional swap under a deadman timer; failure ⇒ nobody
   confirms ⇒ local auto-revert to gen N−1 (re-apply, normal pipeline).
   Base protocol alone already guarantees the weaker form: failure *before*
   commit aborts with nothing applied.
3. *"If all succeed, commit — flagged not-rollback"* → the `Committed` CAS
   flip (base), plus `Confirmed` after the convergence window (Model B).
4. *"Posted node acks all nodes answered OK"* → no extra round exists or is
   needed: the terminal rollout row **is** the broadcast; the admin API call
   observes it and returns the outcome (extending the existing txn-commit
   response contract).

## 7. Sources

Fencing/locking: Kleppmann, *How to do distributed locking* (+ DDIA ch. 8–9);
Chubby (Burrows 2006). Commit theory: Gray & Lamport, *Consensus on
Transaction Commit*; Skeen 3PC critique (partition unsafety). DynamoDB: AWS
Builders' Library *Leader election in distributed systems* (Brooker);
conditional-writes / read-consistency / transactions developer guide;
awslabs dynamodb-lock-client; DynamoDB USENIX ATC'22. Config distribution:
Envoy xDS protocol + CSDS; ZooKeeper dynamic reconfiguration docs + Shraer
et al. USENIX ATC'12; Raft paper + Ongaro thesis §4 + raft-dev single-server
bug thread (2015); Kafka KIP-500/595/631; Kubernetes Deployment docs; Argo
Rollouts; etcd API guarantees; Consul blocking queries / consistency /
sessions. Confirmed-commit family: RFC 6241 §8.3–8.4; RFC 8342 (NMDA);
JunOS commit topic-map + NETCONF confirmed-commit guide; IOS-XR commit/
rollback (+ cisco.iosxr issues #199/#260); Cisco NSO transactions / device
manager / NED development / commit-queue material. Progressive delivery:
LaunchDarkly guarded rollouts; expand-contract (parallel change) pattern.
