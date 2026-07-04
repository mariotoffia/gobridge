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
| [0006](0006-dlq-redrive-at-most-once.md) | DLQ redrive at-most-once | accepted |
| [0007](0007-cluster-worker-seeding-adoptvalid.md) | Cluster worker seeding: AdoptValid default | accepted |
| [0008](0008-cross-hop-identity-lift.md) | Cross-hop bridge-to-bridge identity lift | accepted |

## Numbering

ADRs are numbered sequentially from `0001`. Filenames are `NNNN-<slug>.md`. A
superseded ADR keeps its number and gains a `Superseded by NNNN` line; it is
never deleted or renumbered.
