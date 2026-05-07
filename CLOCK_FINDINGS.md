# Clock Injection PR — Deep Review Findings

PR: `fix/clock-injection` → `main` (22 commits, 98 files, +2878 / −522)
Scope reviewed: every diffed `.go` file across domain, circuitbreaker,
bridge, runtime, httpapi, and the AMQP / AWS / Azure / HTTP / MQTT
adapters; the `domain/clock` interface; `.golangci.yml` forbidigo
configuration; build (`go build ./...`) and `go vet` clean.

## Executive summary

**Verdict: Approve with follow-ups.**

The sweep is thorough and the structural pattern (clock field +
`WithXxxClock` option + `clock.System` default in the constructor +
no backward-compatibility shims) is applied consistently across ~40
production files. Build and vet are green; production code is free of
direct `time.Now()` outside the `domain/clock` package, `_test.go`,
`ports/storetest/`, and `testutil/` (all explicitly exempted in
`.golangci.yml`). The DLQ no-op `WithClock` shims that were dead code
on `main` were correctly removed (verified: the field was only assigned,
never read).

Real issues to address before/after merge:

- ~~**One latent panic**: `runtime.WithCredentialClock(nil)` clobbers the
  default `clock.System` because the option does not guard against nil
  and the constructor does not re-default after applying options.~~
  **Resolved 2026-05-04.** `WithCredentialClock` now guards `if clk != nil`
  inside the option closure, matching the convention used by every other
  `WithXxxClock` in the PR. Regression test
  `TestNewCredentialResolver_NilClockOptionKeepsDefault` added.
- ~~**Forbidigo gate is narrower than the design intent**: it only bans
  `^time\.Now$`. `time.After` is still used in production
  (`cmd/gobridge/main.go`), and `time.Since` / `time.NewTimer` /
  `time.NewTicker` / `time.Sleep` are not banned at all, so the lint
  rule cannot prevent regressions of the clock-abstraction contract on
  those vectors.~~
  **Resolved 2026-05-07.** `.golangci.yml` now bans `time.Now`,
  `time.After`, `time.NewTimer`, `time.NewTicker`, `time.Sleep`,
  `time.Tick`, `time.Since`, and `time.Until` in production. The single
  remaining production caller (`cmd/gobridge/main.go:waitForSupervisorRuntime`)
  was migrated to take a `clock.Clock` parameter and the binary entry
  point passes `clock.System`.
- ~~**Two more `WithClock` options accept nil unguarded** (HTTP factory,
  native file watcher). Both are de-facto safe today because their
  constructors re-apply the `clock.System` fallback after options run,
  but the inconsistency makes the pattern brittle.~~
  **Resolved 2026-05-07.** Both `WithClock` options now guard `if clk != nil`
  inside the option closure. `NewFactory` additionally re-applies the
  `clock.System` default after options run, matching the convention used
  by every other constructor in the codebase.
- **Defensive `s.clock()` accessors** survived the sweep across most
  AMQP/AWS/Azure/MQTT adapters even though every constructor now
  guarantees a non-nil `s.clk`. The user explicitly flagged "scattered
  defensive `if clk == nil` outside the constructor" as out-of-scope
  for this PR; these helpers are exactly that pattern.
  **Reconsidered 2026-05-07.** A removal sweep was attempted and
  reverted: ~30 unit tests across the AMQP/AWS/Azure/MQTT adapters
  build `Sender`/`Receiver`/`Session` struct literals directly,
  bypassing constructors and leaving `s.clk == nil`. The accessors
  centralize the defense in a single method per type — they are the
  *opposite* of "scattered" defensive checks. Keeping them is the
  right tradeoff for now; tightening would require either a fake-clock
  default in every test struct literal or a test-only constructor.

None of the above blocks correctness for normal callers. No genuine
regression, race, or dropped feature was identified.

## Critical issues

_None._

## Major issues

_None remaining._

### ~~1. `WithCredentialClock(nil)` silently disables the default clock and panics on first use~~ — RESOLVED 2026-05-04

**File:** `runtime/credential_resolver.go:37-43` (option)

Fixed by guarding `clk != nil` inside the option closure, matching the
surrounding convention. Regression test
`TestNewCredentialResolver_NilClockOptionKeepsDefault` asserts that a
nil-clock option leaves the `clock.System` default in place and the
first cache touch does not panic.

```go
func WithCredentialClock(clk clock.Clock) CredentialResolverOption {
    return func(r *CredentialResolver) {
        if clk != nil {
            r.clk = clk
        }
    }
}
```

## Minor issues

### ~~2. `cmd/gobridge/main.go` still calls `time.After` in production~~ — RESOLVED 2026-05-07

**File:** `cmd/gobridge/main.go:172-185`

Fixed by threading `clock.Clock` into `waitForSupervisorRuntime`. The
binary entry point in `main` passes `clock.System`. `.golangci.yml` was
broadened in the same commit to include `time.After`, `time.NewTimer`,
`time.NewTicker`, `time.Sleep`, `time.Tick`, `time.Since`, and
`time.Until`, so this vector is now gated for future code.

### ~~3. Two more `WithClock` options accept nil unguarded~~ — RESOLVED 2026-05-07

**Files:**
- `adapters/http/transport/factory.go` — `WithClock` now guards
  `if clk != nil`, and `NewFactory` re-applies the `clock.System`
  default after options run.
- `adapters/native/config/file/acl_watcher.go` — `WithClock` now
  guards `if c != nil`. The constructor's post-options default was
  already in place.

### 4. Defensive `s.clock()` accessors survive in adapters where `s.clk` is now invariant — RECONSIDERED 2026-05-07

**Status:** Kept by design. A removal sweep was attempted on 2026-05-07
and reverted: ~30 unit tests across the AMQP/AWS/Azure/MQTT adapters
build `Sender`/`Receiver`/`Session` struct literals directly,
bypassing constructors and leaving `s.clk == nil`. The accessors
centralize the defense in a single method per type — they are the
*opposite* of "scattered" defensive checks. Tightening would require
either a fake-clock default in every test struct literal or a
test-only constructor; neither is justified for the modest hot-path
overhead.


## Positive observations

- **Default-clock initialization is correct** in every constructor with
  a `clk` field except the one called out above. Sample: `bridge/supervisor.go:168`,
  `runtime/route_runner.go:120-123`, `runtime/dlq_router.go:86-89`,
  `runtime/outbox_drainer.go:194-196`, `httpapi/server.go:121-123`,
  `httpapi/config_txn.go:60-62`, `circuitbreaker/breaker.go:69`,
  `adapters/native/store/sqliteoutbox/store.go:88`,
  `adapters/aws/store/dynamodboutbox/store.go:104`, etc.
- **No production `time.Now()` survives** in scope: a fresh
  `grep -rn "time\.Now()" --include="*.go"` filtered against
  `domain/clock/`, `_test.go`, `ports/storetest/`, and `testutil/`
  returns zero hits. Build (`go build ./...`) and `go vet ./...` are
  clean.
- **Domain layer stays clock-package-free.** `domain/envelope.go`'s
  `IsExpired` / `RemainingTTL` accept an inline
  `interface{ Now() time.Time }`, which keeps `domain` from importing
  `domain/clock` while still permitting injection. The runtime sites
  (`runtime/route_runner.go:371`, `runtime/outbox_drainer_retry.go:16`)
  pass real `clock.Clock` values, satisfying the structural type.
- **Removed shims justified.** The DLQ `WithClock(func() time.Time)`
  options on memorydlq and dynamodbdlq were verified to be dead code on
  `main` (the `now` function field was only assigned, never read), so
  their removal is not a behavioural regression.
- **Outbox drainer ticker registration is now synchronous before the
  goroutine starts** (`adapters/aws/config/dynamodb/loader.go:262-263`,
  T019), eliminating the previous `time.Sleep` polling race in
  `loader_test.go`. Good determinism win.
- **Cloud-Watch batcher** (`adapters/aws/metrics/cloudwatch/batcher.go`)
  threads the same clock through `add` (line 88) and `drain`
  (line 114) for both the per-datum timestamp and the histogram-flush
  timestamp, so a fake clock can produce reproducible timestamps in
  test.
- **Breaker timing is internally consistent**: `b.openedAt` and
  `b.lastFailureTime` are both written via `b.clk.Now()`, and
  `BeforeRequest` measures elapsed via `b.clk.Since(b.openedAt)` —
  no mixed-clock subtraction.
- **Shared-outbox `OutboxRecord.CreatedAt` and `RouteRunner` end-to-end
  latency** are both driven from `r.clk` (`runtime/route_runner_helpers.go:48-66`,
  `runtime/route_runner.go:327` / `:419`), and the new
  `runtime/route_runner_clock_test.go` exercises both paths with a
  fake clock.

## Per-area notes

### domain

- `domain/envelope.go` API change is breaking-by-design (the
  zero-arg `IsExpired` / `RemainingTTL` are gone, replaced by
  `(clk interface{ Now() time.Time })`), matching the "no
  backward-compatibility shims" constraint. All call sites updated.

### circuitbreaker

- Clean injection. `WithBreakerClock` correctly nil-guards. The
  test-only `ForceStateForTest(state, openedAt)` accepts an arbitrary
  `time.Time` for `openedAt`; if a test passes wall time but the
  breaker holds a fake clock, `b.clk.Since(b.openedAt)` mixes clocks —
  but this is a test seam, not a production path.

### bridge

- `Supervisor` clock plumbed end-to-end through `WithSupervisorClock`
  with the `if c != nil` guard. No issues observed.
- `bridge/reconfig_strategy.go` already had clock injection from a
  prior PR; not modified.

### runtime

- Clock injection thoroughly plumbed through `Runtime` →
  `RouteRunner` / `DLQRouter` / `OutboxDrainer` / `SessionManager` /
  `RouteLocator` (`runtime/bridge_start.go:42`, `:56`, `:80`, `:95`,
  `:130`, `:155`, `:185`).
- See **Major #1** for the `WithCredentialClock(nil)` panic.
- `runtime/credentials_poll.go:69` correctly seeds the jitter RNG from
  `w.clk.Now().UnixNano()` *after* options are applied (T007).
- `runtime/instrumented_transport.go:120-126` keeps the original
  semantics of `Extend(until time.Time)` — it forwards `until` as an
  absolute time and does not re-clock-shift. `until` is sourced from
  the runtime side, which now lives on the same `clk` as the delivery.

### httpapi

- `Server`, `configTxnManager`, admin DLQ purge, `generateTxnID` all
  use `s.clk` / `m.clk`. The transaction expiry timer is created via
  `m.clk.NewTimer(ttl)` (line 105) and the goroutine selects on
  `timer.C()` — fully clock-driven.

### adapters/native

- `sqliteoutbox`, `memorylease`, `memoryoutbox`, `credentials/file`
  all default to `clock.System` and gate their `WithClock` options on
  non-nil. `Expire(ctx, before time.Time)` continues to receive an
  absolute cut-off from the caller, which is correct (the caller owns
  the clock).
- `adapters/native/config/file/watcher.go:WithClock` accepts nil
  unguarded but is recovered by the constructor — see Minor #3.

### adapters/aws

- `dynamodbdlq` / `dynamodblease` / `dynamodboutbox` / `cloudwatch`
  exporter / DDB config loader / SQS sender / SQS receiver / SQS
  delivery: all defaults correct, all `WithClock`/`Clock` config
  fields nil-guarded.
- `cloudwatch/exporter.go:151` uses `e.config.Clock.NewTicker(...)`
  for the flush loop — correctly wired.

### adapters/azure

- `servicebus` sender / receiver / delivery default to `clock.System`,
  use the injected clock for `Now`, `Since`, `After`, `NewTicker`
  (auto-extend lock renewal at `delivery.go:222`).
- See Minor #4 for the `s.clock()` accessor.

### adapters/amqp

- `amqp091` and `amqp10` sessions thread `opts.Clock` through to
  senders/receivers/deliveries, with the constructor falling back to
  the session's clock if the per-component config omits one
  (`amqp091/sender.go:43-48`, `amqp10/sender.go:64-69`). This is fine
  because the session's clock is itself defaulted in `applyDefaults`
  (`amqp091/config.go:226-228`).
- See Minor #4 for the `s.clock()` accessor.

### adapters/http

- `factory.go` does not nil-guard `WithClock` and does not default
  `f.clock` in `NewFactory`; the inner `newReceiver` / `newSSESender`
  recover the default. Inconsistent but currently safe — see Minor #3.

### adapters/mqtt/paho

- `session.go:81` defaults `opts.Clock` before storing on `s.clk`.
- See Minor #4 for the `s.clock()` accessor.

## Verification gaps

- **`runtime.WithCredentialClock(nil)`** has no test demonstrating
  default-preservation. The new `runtime/credential_resolver_test.go`
  changes only validate the happy path with a fake clock injected.
  A cheap regression test (`WithCredentialClock(nil)` followed by a
  cache touch) would have caught Major #1.
- ~~**`adapters/http/transport/factory.go`**: no test explicitly verifies
  that `NewFactory(WithClock(nil))` produces a Factory whose
  receiver/sender still uses `clock.System`. The current safety relies
  on inner constructors that could be refactored later.~~
  **Resolved 2026-05-07.** `TestNewFactory_NilClockOptionKeepsDefault`
  and `TestNewFactory_DefaultsToSystemClock` in
  `adapters/http/transport/factory_clock_test.go` assert that both
  no-option and `WithClock(nil)` paths leave `f.clock == clock.System`.
- **`bridge/supervisor.go` SwapEvent.Duration** is now driven by
  `s.clk.Since(start)`. The new test in `bridge/supervisor_test.go`
  asserts the duration is exactly the fake-clock advance, which is
  correct, but no test asserts that swapping mid-flight (e.g.,
  switching `WithSupervisorClock` ... actually impossible after
  construction — fine) preserves consistency.
- **`runtime/instrumented_transport.go:120-126` Extend** is not
  exercised by any production caller (`grep "Extend(ctx" --include="*.go"`
  outside `_test.go` returns only the wrapper definition and the
  adapters' implementations). Not a regression — pre-existing — but
  worth noting.

## Recommended follow-ups

1. ~~**Fix Major #1** — guard `WithCredentialClock` against nil~~
   **Done 2026-05-04.**
2. ~~**Broaden the forbidigo gate** to include `time.After`, `time.Tick`,
   `time.Sleep`, `time.NewTimer`, `time.NewTicker`, `time.Since`,
   `time.Until`~~ **Done 2026-05-07.** Multi-pattern block added to
   `.golangci.yml`; `cmd/gobridge/main.go:waitForSupervisorRuntime` was
   migrated to take a `clock.Clock` parameter rather than carve a new
   exception.
3. ~~**Tidy the defensive `s.clock()` accessors** (Minor #4)~~
   **Reconsidered 2026-05-07.** See Minor #4 for the rationale; the
   accessors are kept as the centralised defense for tests that bypass
   constructors.
4. ~~**Normalize the two outlier `WithClock` options** (Minor #3) so the
   `if c != nil` pattern is universal~~ **Done 2026-05-07.**
5. ~~**Add a `clock.Clock.Sleep(ctx, d)` (or document the
   `<-ctx.Done() / <-clk.After(d)` idiom in the package doc)** so the
   broadened forbidigo rule has a clean alternative for future
   `time.Sleep` callers, particularly the `testutil/` helpers when they
   are eventually pulled in-scope.~~ **Done 2026-05-07.** Documented in
   `domain/clock/clock.go` as a package-level doc with a worked
   `select` example. The `Sleep` method was deliberately not added —
   the comment makes the cancellable idiom canonical.
