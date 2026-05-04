# FIX — SharedOutbox `Complete` must survive batch deadline expiry

> Companion files: `FIX-directhold-retry-pacing.md`,
> `FIX-broker-restart-test-hardening.md`,
> `FIX-docker-exec-timeout.md`.

## Why this exists

When the SharedOutbox drainer successfully sends a record, it must
also call `OutboxStore.Complete` to mark the record done. Today both
operations share the same `batchCtx` derived from the per-batch
`workCtx`:

```go
// runtime/outbox_drainer_loop.go
workCtx, workCancel := context.WithTimeout(
    context.WithoutCancel(ctx), batchTimeout)
batchCtx, batchCancel := context.WithCancel(workCtx)
...
if err := d.processRecord(batchCtx, rec, token); err != nil { ... }
```

Inside `processRecord` (`runtime/outbox_drainer_retry.go`):

```go
sendCtx, sendCancel := context.WithTimeout(ctx, d.policy.SendTimeout)
defer sendCancel()
sendErr := d.sender.Send(sendCtx, env)
...
if sendErr == nil {
    if completeErr := d.outboxStore.Complete(ctx, []string{rec.ID}, token); completeErr != nil {
        d.metrics.Counter(domain.MetricOutboxDuplicateRisk, 1, routeTag)
        ...
        return completeErr
    }
}
```

The bug: when `Send` succeeds **near** the batch deadline (e.g. send
took ~`batchTimeout - 5ms`), the `ctx` passed to `Complete` may be
already expired or have only microseconds left. DynamoDB / SQLite
outbox stores then return `context.DeadlineExceeded`, the record
stays `pending`, and the drainer increments
`MetricOutboxDuplicateRisk` — but the broker has already acked the
message. The next drain re-sends the record, producing a duplicate.

This is reachable in UC42 (broker kill+restart, SharedOutbox) when
the broker is slow during reconnect: a 30 s `SendTimeout` plus
`batchTimeoutFloor` is comfortably under `batchDeadline`, but a
single record can still consume nearly the entire `batchTimeout` in
the recovery window.

## Decision

After a successful `Send`, `Complete` runs under a short, bounded
context that is **derived from the parent runtime context** (so it
inherits cancellation when the runtime stops) but is **not subject
to the batch deadline**.

Two independent constraints:
1. The complete deadline must not be already expired when send
   finishes near the batch deadline.
2. The complete deadline must still be bounded so a stuck
   `OutboxStore.Complete` cannot leak the goroutine forever.

Use a fresh `context.WithTimeout(context.WithoutCancel(parentCtx),
completeTimeout)` where `parentCtx` is the runtime/Start context
plumbed into the drainer via `OutboxDrainer.parentCtx`, and
`completeTimeout` defaults to `min(SendTimeout, 5*time.Second)` (or
configured `OutboxStore.CompleteTimeout` if we add one).

Do the same for the four other `Complete` call sites in
`runtime/outbox_drainer_retry.go`:

- `processRecord` success branch (line 53)
- `processRecord` permanent-error → DLQ branch (line 90)
- `handleExpired` (line 120)
- `handlePoison` (line 139)

All of them have the same exposure: a slow Complete near a tight
batch deadline produces a stuck `pending` record that risks
duplicate delivery.

## Tasks

### 1. Plumb a parent context into the drainer - DONE

`OutboxDrainer` already has access to the runtime via `bridge_start.go`
(see `mgr := newSessionManagerWithMetrics(...)` etc.). Add a field:

```go
// runtime/outbox_drainer.go
type OutboxDrainer struct {
    ...
    parentCtx context.Context // set at Start; not subject to per-batch timeouts
}
```

Set it once at drainer construction (Start path). Use
`context.WithoutCancel(parentCtx)` to derive the complete-context so
runtime shutdown still propagates *intent* to stop, but per-batch
deadlines do not bleed into Complete.

If wiring `parentCtx` is invasive, a smaller alternative: derive the
complete-ctx from `context.WithoutCancel(ctx)` directly in the
success branch — the drained ctx is the runtime's `Start` ctx today.
Verify by reading `runtime/outbox_drainer_loop.go` around the
`workCtx` construction: the `ctx` parameter of `runDrainCycle`
**is** the runtime ctx, so `WithoutCancel(ctx)` is correct.

### 2. Helper for complete-context - DONE

```go
// completeCtx returns a context for OutboxStore.Complete that is not
// affected by the batch deadline. It still propagates runtime
// shutdown (parent cancellation is reflected by the timeout
// expiring) and is bounded so a stuck Complete cannot leak.
func (d *OutboxDrainer) completeCtx(parent context.Context) (context.Context, context.CancelFunc) {
    timeout := d.policy.SendTimeout
    if timeout <= 0 || timeout > 5*time.Second {
        timeout = 5 * time.Second
    }
    return context.WithTimeout(context.WithoutCancel(parent), timeout)
}
```

Replace each of the five `Complete(ctx, ...)` calls with:

```go
cctx, ccancel := d.completeCtx(ctx)
err := d.outboxStore.Complete(cctx, []string{rec.ID}, token)
ccancel()
```

### 3. Regression test - DONE

Add `runtime/outbox_drainer_complete_deadline_test.go`:

1. `TestOutboxDrainer_CompleteSurvivesNearBatchDeadline` — fake
   `Sender` that sleeps for `batchTimeout - 50ms`, then returns nil.
   Fake `OutboxStore.Complete` that records the deadline of the
   context it receives. Assert the recorded deadline is in the
   future (not already past, not within 1 ms of now), and the
   record state transitions to `Completed`.
2. `TestOutboxDrainer_CompleteRespectsRuntimeShutdown` — set the
   runtime ctx to be cancelled; assert `Complete` still runs (because
   it uses `WithoutCancel`) but only up to `completeTimeout`. This
   catches accidentally re-introducing a parent-cancel link.

Run:
```bash
go test -race -run 'TestOutboxDrainer_Complete' ./runtime/...
```

### 4. Verify with broker-restart longrunning targets - DONE

```bash
go test -race -timeout 1200s -v -tags=longrunning \
  -run 'TestUC42_BrokerKillRestart_SharedOutbox' \
  ./tests/longrunning/...
```

Then `make test-long-running`.

## Acceptance

- The five `OutboxStore.Complete(ctx, ...)` call sites in
  `runtime/outbox_drainer_retry.go` use `completeCtx` (or equivalent).
- A regression test fails on the old code (Complete ctx already
  expired) and passes on the new code.
- `MetricOutboxDuplicateRisk` no longer fires on the
  send-near-deadline scenario.
- `make test` and `make test-long-running` are green.

## Non-goals

- No change to `OutboxStore.Complete` interface — it already accepts
  a context.
- No change to the per-batch `workCtx`/`batchCtx` design; that
  bounds Send, which is correct.
- Do not extend `completeTimeout` beyond ~5 s — a slow Complete
  should be observable, not absorbed.

## Related

- `runtime/outbox_drainer_loop.go` — `runDrainCycle` /
  `batchDeadline`.
- `runtime/outbox_drainer_retry.go` — `processRecord`,
  `handleExpired`, `handlePoison`.
- `runtime/outbox_drainer.go` — drainer struct/config wiring.
