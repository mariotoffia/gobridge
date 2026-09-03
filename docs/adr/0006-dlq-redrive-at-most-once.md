# 0006 — DLQ redrive at-most-once

Status: superseded by [0015](0015-dlq-redrive-inject-then-delete.md) — the shipped redrive injects first and deletes after a confirmed inject (at-least-once); the claim-by-delete design below is no longer what the code does
Date: 2026-07-03
Deciders: GoBridge core

## Context

DLQ redrive replays a dead-lettered message back onto its route. The operation
runs from the admin API under conditions that are hostile to naive replay:

- The operator's HTTP request can disconnect mid-batch.
- The operator can retry a redrive that already partly ran.
- Two admin instances can redrive the same entry concurrently.
- The failed message came from one binding on a route that may fan out to
  several.

Replaying the message and then deleting the entry risks double delivery under
retries and concurrent redrives. Injecting it back onto the whole route
re-delivers it to the N-1 bindings that never failed.

## Decision

Claim by deleting first, inject second, and confine the replay to the one
binding that failed. The design lives in `httpapi/admin_dlq.go`; the wire
contract is documented in `spec/httpapi/components.yaml:425-455`.

- **Claim-by-delete-first.** For each entry, `Delete` runs before `inject`
  (`admin_dlq.go:365-380`). A re-sent request finds the entry already gone
  (`deleted == 0`) and skips it instead of re-injecting (`:381-387`). Two admin
  instances race on `Delete`; only the one whose delete returns 1 owns the entry
  and injects it.

- **At-most-once bias.** Deleting first trades an at-most-once window — a crash
  between delete and inject drops the entry — for no double delivery
  (`admin_dlq.go:369-372`). For a manual redrive, losing a message is preferable
  to delivering it twice.

- **Detached restore contexts.** The claim→inject sequence runs under a context
  detached from the request (`context.WithoutCancel` + a bounded
  `redriveTimeout`, `admin_dlq.go:342-348`) so an operator disconnect cannot
  cancel an in-flight restore and permanently lose a claimed entry. On inject
  failure, the best-effort restore runs under its own fresh detached context, not
  the per-batch one, so a batch that exhausted its budget mid-loop does not fail
  the restore on an already-expired context (`admin_dlq.go:402-410`).

- **Binding-scoped confinement.** The entry records the exact `BindingID` that
  failed. Redrive carries that binding out-of-band via `Runtime.InjectToBinding`
  (a typed field, not a header — see ADR 0001), confining the replay to that one
  binding (`admin_dlq.go:389-397`). The N-1 healthy bindings on a fan-out route
  receive no duplicate.

- **Missing binding preserves the entry.** When the recorded `BindingID` no
  longer exists on a still-present but reconfigured route, `InjectToBinding`
  refuses **before** the pipeline runs and returns `ErrNotFound` (permanent)
  rather than falling back to a full-route fan-out that would duplicate-deliver
  to the N-1 healthy legs (`runtime/route/runner.go:415-421`). The redrive is
  reported as failed with the per-entry message `route or binding not found`,
  and the claimed DLQ entry is best-effort restored so the failure evidence is
  kept for a later re-file (`admin_dlq.go:421-440`). This is deliberately
  distinct from an in-band processor `ActionRoute` override, whose
  unknown-binding fall-through to normal resolution is intentional.

- **Partial-failure status.** A redrive returns HTTP 200 when every requested
  entry redrove, or HTTP 207 Multi-Status when any entry failed, so the caller
  inspects the per-entry result (`admin_dlq.go:426`, `:447-451`;
  `components.yaml:451-455`).

## Consequences

- Redrive never double-delivers, under client retries or concurrent admin
  instances. The claim-by-delete makes ownership exclusive.
- A crash in the delete→inject window loses the claimed entry. The best-effort
  restore closes most of that window but not all of it; the residual is the
  accepted cost of the at-most-once bias.
- A fan-out route redrives only the failed leg. Healthy destinations see nothing.
- Callers must inspect the 207 body — a 2xx does not imply every entry redrove.

## Rejected alternatives

- **Inject first, delete after success.** At-least-once: a retry or a
  post-inject crash re-injects an entry that already delivered. Rejected for a
  manual operation where duplicate delivery is worse than a lost message the
  operator can re-file.
- **Redrive to the whole route.** Re-delivers to bindings that never failed.
  Binding-scoped injection via `InjectToBinding` avoids the collateral fan-out.
- **Tie the restore to the request context.** An operator disconnect would then
  abort the restore and lose the already-deleted entry. Detached contexts exist
  precisely to prevent that.
