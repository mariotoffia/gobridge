# Cluster protocol — specification

This folder is the technical specification for GoBridge's **coordinated cluster
config rollout** — how a cluster applies a config change all-or-nothing, with
per-node acknowledgement, and (optionally) with an automatic rollback if the
change cannot actually reach its brokers.

If you just want to turn clustering on and pick a mode, read the plain-language
[configuration guide](../README.md) instead. If you operate a live cluster, read
the [operations runbook](../../runbooks/cluster-config-rollout.md). This folder
is the *design*.

## Contents

| File | What it is |
|---|---|
| [cluster-config-rollout-protocol.md](cluster-config-rollout-protocol.md) | The canonical protocol design: problem, goals, the barrier state machine, domain model, ports, orchestration, the failure matrix, rollout classes, and the confirm-window extension (§8.1). |
| [cluster-config-rollout-confirm-window.md](cluster-config-rollout-confirm-window.md) | The confirm-window (§8.1) code-level implementation spec and the two decisions it finalized (revert target, confirm-within-window). |
| [cluster-config-rollout-research.md](cluster-config-rollout-research.md) | Prior-art digest and split-brain analysis — the primary-source study the design is built on (RFCs, papers, vendor docs). |

The **shipped** decisions are recorded as ADRs, which are authoritative for what
runs: [ADR 0012](../../adr/0012-cluster-config-whole-cohort-replacement.md)
(whole-cohort replacement — the default),
[ADR 0013](../../adr/0013-coordinated-cluster-config-rollout.md) (the coordinated
barrier), and
[ADR 0014](../../adr/0014-confirm-window-provisional-commit.md) (the confirm
window). Where a spec and an ADR disagree, the ADR wins.

## What we borrow from other specifications

GoBridge does not invent a new consensus system. The design deliberately reuses
established, battle-tested mechanisms and reads out to the source specs. This is
the map of **which idea comes from where, and where it lands in our design** —
the full analysis (with citations) is in
[cluster-config-rollout-research.md](cluster-config-rollout-research.md).

| Source spec / system | What we take from it | Where it lands |
|---|---|---|
| **NETCONF confirmed-commit** — RFC 6241 §8.4 | Provisional apply with a deadman timer; a confirming commit within the window makes it permanent, else it MUST revert; a reboot before confirmation reverts to the prior config. | The confirm window (protocol §8.1, ADR 0014): provisional swap, `confirm_window` deadman, "reboot onto the last confirmed generation". |
| **Cisco NSO** — `commit confirmed`, network-wide transaction | Abort-by-inaction: on failure, *don't confirm the rest* and their timers revert them; revert is applied through the **normal** transaction path, not a special one. | Deadman revert = re-apply generation N−1 through the same apply pipeline (protocol §8.1, research rule 9). |
| **RFC 8342 (NMDA)** | *Intended* vs *operational* datastore — accepted ≠ applied; convergence is measured by diffing them. | The convergence check that a member must pass before it records `Converge`. |
| **Envoy xDS + CSDS** | Versioned config; every ACK/NACK names the version; a NACKing node keeps its last-good config and keeps serving. | Digest+version on the rollout row; `Ack`/`Nack`; the old config keeps serving on abort. |
| **ZooKeeper dynamic reconfig** (Shraer, ATC'12) | Exactly one config change in flight; precondition the cohort before committing. | I1 (one active rollout via CAS); the frozen membership epoch. |
| **Raft membership** (Ongaro §4) | One change at a time with quorum overlap; abort the change if a member can't come along; fall back to the prior config entry. | Freeze-and-abort on membership change (F6); no mixed-version cohort (G2). |
| **Kafka KRaft** | Per-node applied-state is a durable record (an offset heartbeat), never coordinator memory; the option to fence laggards out of serving. | Per-member `Ack`/`Converge` records in the store; the Kafka-style fence is the deferred Q4 alternative. |
| **Kubernetes / Argo Rollouts** | Auto-rollback is an **opt-in layer** above the base rollout, never the base itself. | The confirm window is opt-in and off by default; the base barrier commits without it. |
| **Chubby** (Burrows, 2006) | Lock-delay: a new lock holder waits out the previous lease before acting. | A freshly elected coordinator waits one previous-lease duration before its first side effect. |
| **Fencing & commit theory** — Kleppmann; Gray & Lamport, *Consensus on Transaction Commit* | Fencing tokens reject stale actors; "2PC with the decision in a linearizable store" is Paxos Commit (the coordinator becomes disposable). | `LeaseToken` fencing (I3); the store row is the commit point, so a dead coordinator is resumed, not repaired (F3). |
| **DynamoDB** conditional writes (single-Region, multi-AZ, internally Paxos) | Per-item compare-and-set as the atomicity primitive; consistent reads as truth. | Every authoritative transition is a conditional write; split-brain is prevented by construction (research §3). |
| **LaunchDarkly guarded rollouts / expand-contract** | Trial window + automated health verdict + promote-or-revert. | The shape of the confirm window: apply on trial, confirm or auto-revert. |

## See also

- [Plain-language configuration guide](../README.md) — which mode to choose and
  what each one gives you.
- [Cost guide (TCO)](../tco.md) — what each mode costs in dollars and operational
  effort, with reference scenarios and a decision matrix.
- [Operations runbook](../../runbooks/cluster-config-rollout.md) — posting
  changes, reading rollout health, recovering a stuck rollout.
