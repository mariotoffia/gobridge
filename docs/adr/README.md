# Architecture Decision Records

Records of decisions already made and shipped on `main`. Each ADR captures the
context, the decision, and its consequences, so the reasoning survives past the
commit that carried the code.

Format: MADR-style. Status is `accepted` for every record here — these document
behavior that already ships, not proposals.

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-reserved-header-trust-model.md) | Reserved-header trust model and out-of-band signaling | accepted |
| [0002](0002-credential-rotation-build-first.md) | Credential rotation: build-first, commit-after-success | accepted |
| [0003](0003-mqtt-persistent-session-hygiene.md) | MQTT persistent-session subscription hygiene | accepted |
| [0004](0004-single-use-runtime-lifecycle.md) | Single-use runtime lifecycle and terminal wedge | accepted |
| [0005](0005-outbox-partition-claim-design.md) | Outbox partition design: claim selection, fence rows, seq allocation | accepted |
| [0006](0006-dlq-redrive-at-most-once.md) | DLQ redrive at-most-once | superseded by 0015 |
| [0007](0007-cluster-worker-seeding-adoptvalid.md) | Cluster worker seeding: AdoptValid default | superseded by 0012 |
| [0008](0008-cross-hop-identity-lift.md) | Cross-hop bridge-to-bridge identity lift | accepted |
| [0009](0009-durable-outbound-mqtt-session-state.md) | Durable outbound MQTT session state: in-memory store, route-layer durability | accepted |
| [0010](0010-mqtt-loop-prevention-contract.md) | MQTT bridge-to-bridge loop-prevention contract | accepted |
| [0011](0011-cluster-client-id-uniqueness.md) | Cluster client-ID uniqueness enforcement | accepted |
| [0012](0012-cluster-config-whole-cohort-replacement.md) | Cluster config changes require whole-cohort replacement | superseded by 0013 (live-safe deltas) |
| [0013](0013-coordinated-cluster-config-rollout.md) | Coordinated cluster config rollout for live-safe deltas | accepted |
| [0014](0014-confirm-window-provisional-commit.md) | Confirm window: provisional commit with deadman revert | accepted |
| [0015](0015-dlq-redrive-inject-then-delete.md) | DLQ redrive inject-then-delete (at-least-once) | accepted |

## Numbering

ADRs are numbered sequentially from `0001`. Filenames are `NNNN-<slug>.md`. A
superseded ADR keeps its number and gains a `Superseded by NNNN` line; it is
never deleted or renumbered.
