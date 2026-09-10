# 0015 — DLQ redrive inject-then-delete (at-least-once)

Status: accepted
Date: 2026-09-03
Deciders: GoBridge core
Supersedes: [0006](0006-dlq-redrive-at-most-once.md) (claim-by-delete, at-most-once)

## Context

ADR 0006 chose claim-by-delete for `POST /api/v1/admin/dlq/redrive`: delete the
entry first, inject second, and accept that a crash in between loses the entry.
The rationale was that for a manual recovery action a lost message was
preferable to a duplicate. Two things about that trade turned out to be wrong in
production terms:

- **The loss is unrecoverable, and unrecorded.** The DLQ entry is the only
  durable record that the message existed and was given up on. Deleting it
  before the inject is confirmed means a crash, a SIGKILL or a store outage in
  that window erases both the message and the evidence. The `dlq.redrive.begin`
  audit record names entry IDs, not payloads, so nothing can be re-filed from it.
- **A replay that reused the original identity could be swallowed silently.**
  The outbox's retained dedup row (for a `shared_outbox` fan-out leg) and an
  idempotent or FIFO transport's dedup key (for a direct entry) both treat a
  re-persist under the original envelope ID as a duplicate. `Send` returns nil,
  the handler reports success and deletes the entry after a no-op: silent loss
  via dedup rather than via a crash.

## Decision

Inject first, delete only after a confirmed inject, and re-issue every redrive
under a fresh identity confined to the binding that failed.

- **Inject-then-delete.** For each entry `handleDLQRedrive` runs `DLQReader.Get`,
  then `Runtime.InjectRedrive`, then `DLQAdmin.Delete`. A failed or refused
  inject leaves the entry — and its failure evidence — fully intact. A failed
  delete after a confirmed inject is reported per entry as "message re-injected
  but DLQ entry not removed"; the operator removes it by hand rather than
  believing the redrive failed. A deleted count of zero (a concurrent redrive
  removed it first) is benign: the inject happened.

- **At-least-once.** The cost of the ordering is a bounded duplicate window: a
  crash between a confirmed inject and the delete re-drives the entry on the
  next attempt. For a manual recovery action, never losing the message is the
  correct bias; downstream consumers must already be idempotent because every
  GoBridge delivery path is at-least-once.

- **Fresh identity with provenance.** `Runtime.InjectRedrive` mints a new
  envelope ID and stamps a causation link to the original, so the outbox dedup
  index sees a NEW delivery attempt while the audit trail survives. It strips
  what would otherwise sink or swallow the replay: the transport dedup key
  (`x-bridge.dedup-id`, which an idempotent sender prefers over the envelope
  ID), the generated-identity marker (which makes a message uncountable by the
  replay ledger, so it is sunk on its first transient failure), and the source
  transport's redelivery counter (`route.StripInboundReceiveCounts`; the entry
  being redriven is usually one that counter poisoned).

- **Binding-scoped confinement.** The failed `BindingID` is carried out-of-band
  through `InjectRedrive`, never as a header (the ingress reserved-header strip
  would remove it), so a fan-out route re-delivers only to the leg that failed.
  A recorded binding that no longer exists is refused before the pipeline runs
  (`ErrNotFound`, reported as "route or binding not found") rather than falling
  back to a whole-route fan-out.

- **A dropped or re-dead-lettered redrive is a failure, not a success.** A
  synthetic delivery learns that it was settled terminally without delivery and
  `InjectRedrive` returns an error wrapping `ports.ErrInjectNotDelivered`; the
  entry is kept, the failure is counted on `DLQRedriveFailures`, and the
  per-entry error names the cause.

- **Runtimes without redrive-safe injection refuse.** When the runtime does not
  implement `InjectRedrive`, a binding-scoped entry and any direct entry that
  carries a non-empty ID or a dedup key are refused before any inject, with the
  entry preserved. Only a collision-free direct entry (empty ID, no dedup
  header) falls back to a plain inject.

- **Detached, bounded context.** The inject-then-delete sequence runs under
  `context.WithoutCancel` plus `redriveTimeout`, so an operator disconnect
  mid-batch cannot cancel the delete that follows a confirmed inject and cause
  a duplicate on the next attempt.

- **Partial-failure status** is unchanged from ADR 0006: HTTP 200 when every
  requested entry redrove, HTTP 207 Multi-Status when any failed.

## Consequences

- No redrive path deletes evidence before delivery is confirmed. Loss through
  the redrive endpoint requires losing the DLQ store itself.
- Duplicates are possible, bounded to the inject→delete window and reported
  when the delete fails. This matches every other delivery guarantee in the
  system.
- The OpenAPI description (`spec/httpapi/components.yaml`, `DLQRedriveRequest`),
  `docs/http-api.md`, `docs/release-notes.md` and the `DLQStore` section of
  `ARCHITECTURE.md` describe this contract; a test in `httpapi` and one in
  `ports` fail if any of them drifts back to the ADR 0006 wording.

## Rejected alternatives

- **Keep claim-by-delete (ADR 0006).** Rejected: the at-most-once window
  erased the only record of the loss, and the original-identity replay was
  silently deduplicated on both the outbox and transport paths.
- **Inject under the original envelope ID.** Rejected for the dedup reasons
  above; a fresh ID with a causation link keeps the audit trail without
  reusing the identity dedup keys on.
- **Whole-route replay.** Rejected as in ADR 0006: the healthy N-1 bindings
  would receive duplicates.
