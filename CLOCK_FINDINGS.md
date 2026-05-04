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
- **Forbidigo gate is narrower than the design intent**: it only bans
  `^time\.Now$`. `time.After` is still used in production
  (`cmd/gobridge/main.go`), and `time.Since` / `time.NewTimer` /
  `time.NewTicker` / `time.Sleep` are not banned at all, so the lint
  rule cannot prevent regressions of the clock-abstraction contract on
  those vectors.
- **Two more `WithClock` options accept nil unguarded** (HTTP factory,
  native file watcher). Both are de-facto safe today because their
  constructors re-apply the `clock.System` fallback after options run,
  but the inconsistency makes the pattern brittle.
- **Defensive `s.clock()` accessors** survived the sweep across most
  AMQP/AWS/Azure/MQTT adapters even though every constructor now
  guarantees a non-nil `s.clk`. The user explicitly flagged "scattered
  defensive `if clk == nil` outside the constructor" as out-of-scope
  for this PR; these helpers are exactly that pattern.

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

### 2. `cmd/gobridge/main.go` still calls `time.After` in production

**File:** `cmd/gobridge/main.go:172-185`

```go
func waitForSupervisorRuntime(sup *bridge.Supervisor, timeout time.Duration) *goruntime.Runtime {
    // ESSENTIAL: runtime init poll
    deadline := time.After(timeout)
    for {
        if rt := sup.Runtime(); rt != nil {
            return rt
        }
        select {
        case <-deadline:
            return nil
        case <-time.After(20 * time.Millisecond):
        }
    }
}
```

Forbidigo is configured to ban only `^time\.Now$`, so this slips through.
The PR's stated goal — "every production timestamp / deadline / TTL
calculation goes through a `clock.Clock`" — is technically violated
here, and the same vector (`time.After`/`time.NewTimer`/`time.NewTicker`/
`time.Since`/`time.Sleep`) is unprotected anywhere in future code.
`cmd/gobridge/main.go` was not in the FIX-TODO Phase-1 caller list, so
this is a missed site rather than a regression.

**Suggested fix:** Either accept this gap explicitly in the
FIX-TODO note, or thread a clock through `waitForSupervisorRuntime`
and broaden `forbidigo` to include `time.After|time.NewTimer|time.NewTicker|time.Sleep|time.Tick|time.Since|time.Until`.

### 3. Two more `WithClock` options accept nil unguarded

**Files:**
- `adapters/http/transport/factory.go:61-63`
  ```go
  func WithClock(clk clock.Clock) FactoryOption {
      return func(f *Factory) { f.clock = clk }
  }
  ```
  And `NewFactory` (line 65-75) does not re-default `f.clock` to
  `clock.System` after applying options. The inner `newReceiver` /
  `newSSESender` constructors do default a nil `cfg.clock`, so the
  current end-state is correct, but the Factory itself can hold a nil
  clock — surprising for any future consumer that reads `f.clock`
  directly.
- `adapters/native/config/file/watcher.go:87-89`
  ```go
  func WithClock(c clock.Clock) WatcherOption {
      return func(w *Watcher) { w.clk = c }
  }
  ```
  Safe today because `NewWatcher` re-applies the `clock.System` fallback
  after options (line 130-132), but inconsistent with every other
  `WithXxxClock` introduced by this PR.

**Suggested fix:** Add `if c != nil` to both options for consistency,
or add the post-options `if f.clock == nil { f.clock = clock.System }`
guard to `NewFactory` (and remove the inner-constructor fallbacks if
the field becomes invariant).

### 4. Defensive `s.clock()` accessors survive in adapters where `s.clk` is now invariant

**Files:**
- `adapters/aws/transport/sqs/sender.go:64-69`
- `adapters/aws/transport/sqs/receiver.go:60-65`
- `adapters/azure/transport/servicebus/sender.go:63-68`
- `adapters/azure/transport/servicebus/receiver.go:55-59`
- `adapters/amqp/transport/amqp091/sender.go:62-67`
- `adapters/amqp/transport/amqp091/receiver.go:62-67`
- `adapters/amqp/transport/amqp091/session.go:111-116`
- `adapters/amqp/transport/amqp10/sender.go:79-84`
- `adapters/amqp/transport/amqp10/receiver.go:77-82`
- `adapters/amqp/transport/amqp10/session.go:107-112`
- `adapters/mqtt/transport/paho/session.go:97-102`

Sample (`adapters/aws/transport/sqs/sender.go`):

```go
clk := cfg.Clock
if clk == nil {
    clk = clock.System
}
return &Sender{ ..., clk: clk }, nil
}

func (s *Sender) clock() clock.Clock {
    if s.clk != nil {
        return s.clk
    }
    return clock.System
}
```

The constructor (and `applyDefaults` for sessions, e.g. `amqp091/config.go:226-228`,
`mqtt/paho/session.go:81`) already guarantees `s.clk != nil`. The
`s.clock()` accessor is therefore the kind of "scattered defensive
`if clk == nil { clk = clock.System }` outside the constructor" that
the user's hard constraint flagged. Each call site (~30 across the
adapters) goes through an extra method call and nil-check on every hot
path (every Send / Receive / poll iteration / session event) for no
behavioural benefit.

**Suggested fix:** Drop the accessor and reference `s.clk` directly in
all listed files, since the constructor invariant holds. (The same
applies inside `runtime/instrumented.go:109-114`'s
`instrumentedClock` helper, but its callers pass clocks from external
boundaries where defending against nil is at least defensible.)

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
- **`adapters/http/transport/factory.go`**: no test explicitly verifies
  that `NewFactory(WithClock(nil))` produces a Factory whose
  receiver/sender still uses `clock.System`. The current safety relies
  on inner constructors that could be refactored later.
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

1. **Fix Major #1** — guard `WithCredentialClock` against nil (one-line
   change) before this PR merges.
2. **Broaden the forbidigo gate** to include `time.After`, `time.Tick`,
   `time.Sleep`, `time.NewTimer`, `time.NewTicker`, `time.Since`,
   `time.Until`. Concretely: a multi-pattern block in
   `.golangci.yml`. Also add `cmd/gobridge/main.go:waitForSupervisorRuntime`
   to the migration list (or accept it as a documented exception for
   the binary entry point, since `Supervisor.Runtime()` initialization
   is intrinsically wall-clock-bounded for human-interactive startup).
3. **Tidy the defensive `s.clock()` accessors** (Minor #4) in a
   follow-up: if the constructor invariant holds, the accessor is dead
   weight on every hot-path send / receive / poll cycle and contradicts
   the user's stated convention.
4. **Normalize the two outlier `WithClock` options** (Minor #3) so the
   `if c != nil` pattern is universal — easier to maintain than a mix.
5. **Add a `clock.Clock.Sleep(ctx, d)` (or document the
   `<-ctx.Done() / <-clk.After(d)` idiom in the package doc)** so the
   broadened forbidigo rule has a clean alternative for future
   `time.Sleep` callers, particularly the `testutil/` helpers when they
   are eventually pulled in-scope.
