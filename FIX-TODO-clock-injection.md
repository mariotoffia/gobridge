# FIX-TODO — Clock-injection sweep (gates `forbidigo`)

> Carve-out from the architectural TODOs that survived the
> April 2026 sprint. Companion files: `FIX-004.md` (domain split),
> `FIX-006.md` (adapter ACL refactor), and the other
> `FIX-TODO-*.md` documents in this folder.

## Why this exists

The `forbidigo` golangci-lint rule that bans `time.Now` (in favour of
an injected `domain/clock.Clock`) is **disabled** today because ~40
production files call `time.Now()` directly. Enabling the rule
without first migrating those callers would force ~40 file-level
exemptions and then the rule would have no teeth on new violations
either — it would only catch what we forgot to exempt.

The architectural goal: every production timestamp / deadline / TTL
calculation goes through a `clock.Clock` so deterministic tests
(via `clocktest.Fake`) become possible without `time.Sleep`-driven
race windows.

## Current state (snapshot at FIX-009)

- `domain/clock.Clock` interface defined; `clock.System` is the
  real-time impl; `clock/clocktest.Fake` is the test impl.
- Some adapters already inject the clock (the DDB loader `pollLoop`
  uses `l.clk.NewTicker` for example).
- `.golangci.yml` has the `forbidigo` block configured but commented
  out; once enabled it bans `^time\.Now$` with the message
  `"use clock.Now via dependency injection (see domain/clock)"`.
- Existing exemptions in `.golangci.yml` (kept for the eventual
  re-enable):
  - `domain/clock/real.go` — the implementation itself
  - `domain/clock/clocktest/` — fake-clock helpers
  - `_test.go` files — tests use `time.Now` for "right now" semantics
- One specific TODO points here:
  `adapters/aws/config/dynamodb/loader_test.go:229` — uses
  `time.Sleep(500ms)` and `//nolint:forbidigo` to wait for poll
  cycles that could be driven by a fake-clock advance.

### Production callers of `time.Now()` (snapshot)

```text
./adapters/amqp/transport/amqp091/{delivery,receiver,sender,session}.go
./adapters/amqp/transport/amqp10/{delivery,receiver,sender,session}.go
./adapters/aws/metrics/cloudwatch/batcher.go
./adapters/aws/transport/sqs/{delivery,receiver,sender}.go
./adapters/azure/transport/servicebus/{delivery,receiver,sender}.go
./adapters/http/transport/{receiver,sender_sse}.go
./adapters/mqtt/transport/paho/{headers,sender,session_health,session_lifecycle,session_reconcile}.go
./adapters/native/credentials/file/repository.go
./adapters/native/store/sqliteoutbox/store.go
./bridge/supervisor.go
./circuitbreaker/breaker.go
./domain/envelope.go
./httpapi/{admin,admin_dlq,config_txn,server}.go
./ports/storetest/outbox.go         (conformance fixture; treat like test)
./runtime/{credential_resolver,credentials_poll,dlq_router,
           instrumented,instrumented_transport,outbox_drainer_loop,
           route_runner,route_runner_helpers,session_manager,
           session_manager_lease}.go
```

≈ 40 production files. Most are timestamping (record creation time,
expiry calculation), some are deadline math (visibility timeout
extension), some are scheduler internal state.

## Approach

### Phase 1 — Harvest the callers - DONE

**Status:** Resolved 2026-05-04. Re-ran the caller harvest and classified the remaining direct `time.Now()` production uses so the package-by-package injection tasks can proceed without re-discovery.

For each file in the list above, classify the `time.Now()` call:

- **Timestamp on a domain value** (e.g. `OutboxRecord.CreatedAt`).
  → The caller already owns the value; inject a clock at construction
    time or accept it as a parameter.
- **Deadline math** (e.g. visibility timeout = now + 30s).
  → Inject a clock into the surrounding type (struct field).
- **Scheduler / timer internal** (e.g. last-tick comparison).
  → Already drivable by `clk.NewTicker` / `clk.NewTimer`; rewrite to
    use `clk.Now()` instead of `time.Now()`.

Harvest result (production paths, excluding `_test.go`, `domain/clock/real.go`, and `domain/clock/clocktest/`):

- **Timestamp / domain value creation:** `*/receiver.go` and `adapters/mqtt/transport/paho/headers.go` `CreatedAt` assignments, `runtime/dlq_router.go` `FailedAt`, `runtime/session_manager.go` and transport session `ports.SessionEvent.Timestamp`, `httpapi/admin.go`, `httpapi/config_txn.go` transaction timestamps/IDs, `adapters/native/credentials/file/repository.go` `UpdatedAt`, and `adapters/native/store/sqliteoutbox/store.go` record timestamps.
- **Deadline / expiry math:** `domain/envelope.go` expiry checks, `adapters/azure/transport/servicebus/delivery.go` lock/schedule checks, `httpapi/admin_dlq.go` purge cutoff, `httpapi/config_txn.go` active transaction expiry, `runtime/credential_resolver.go` cache TTLs, `runtime/route_runner_helpers.go` retry/backoff timing, `ports/storetest/outbox.go` conformance expiry fixtures, and SQLite outbox expiry update time.
- **Scheduler / internal timing and metrics:** transport send/receive/ack/connect/reconcile duration measurements across AMQP 0.9.1, AMQP 1.0, SQS, Azure Service Bus, HTTP, MQTT; `adapters/aws/metrics/cloudwatch/batcher.go`; `bridge/supervisor.go`; `circuitbreaker/breaker.go`; `httpapi/server.go`; `runtime/credentials_poll.go`; `runtime/instrumented*.go`; `runtime/outbox_drainer_loop.go`; `runtime/route_runner.go`; and `runtime/session_manager_lease.go`.
- **Out of production-gate scope but still present:** `testutil/**` local-service helpers use real time for unique names and polling deadlines; keep them out of the production `forbidigo` gate unless the lint scope is later expanded.

**What landed:**

- [FIX-TODO-clock-injection.md](FIX-TODO-clock-injection.md) records the completed caller harvest and classification groups.

**Tests added:**

- none — documentation/indexing-only task.

**Pre-existing issues fixed in touched files (per audit instruction):**

- none.

**Follow-ups (not blockers; logged for future passes):**

- none.

**Agents/Skills used:** golang-pro, code-reviewer.

### Phase 2 — Inject clocks per package

**Status:** Resolved 2026-05-04. Clock injection is established for native/AWS outbox and lease stores with `clock.Clock` fields defaulting to `clock.System`; no-op DLQ clock compatibility shims were removed per the no-backward-compatibility directive.

Each package containing time-dependent code gets one of:

- A struct field `clk clock.Clock` defaulting to `clock.System`.
- A constructor parameter / functional option `WithClock(c clock.Clock)`
  (already the convention in the DDB loader, MQTT session, etc.).

The injection MUST default to `clock.System` so callers that don't
opt in keep working. Tests opt in via `WithClock(clocktest.New())`.

**What landed:**

- Added `clock.Clock` injection to [adapters/native/store/memoryoutbox/store.go](adapters/native/store/memoryoutbox/store.go), [adapters/native/store/memorylease/store.go](adapters/native/store/memorylease/store.go), [adapters/native/store/sqliteoutbox/store.go](adapters/native/store/sqliteoutbox/store.go), [adapters/aws/store/dynamodblease/store.go](adapters/aws/store/dynamodblease/store.go), and [adapters/aws/store/dynamodboutbox/store.go](adapters/aws/store/dynamodboutbox/store.go).
- Removed unused/no-op `WithClock(func() time.Time)` APIs from [adapters/native/store/memorydlq/store.go](adapters/native/store/memorydlq/store.go) and [adapters/aws/store/dynamodbdlq/store.go](adapters/aws/store/dynamodbdlq/store.go).
- Replaced SQLite outbox logger mutation with constructor options, including `WithClock` and `WithLogger`.

**Tests added:**

- Added SQLite outbox injected-clock coverage in [adapters/native/store/sqliteoutbox/store_test.go](adapters/native/store/sqliteoutbox/store_test.go).
- Updated native memory lease/DLQ tests to use `clocktest` fake clocks.

**Pre-existing issues fixed in touched files (per audit instruction):**

- Removed misleading DLQ clock options that were never consulted by the stores.

**Follow-ups (not blockers; logged for future passes):**

- `go-core/PACKAGES.md` was not present in the repository.
- Broader transport/runtime `time.Now()` sweeps remain for later phase-specific tasks.

**Agents/Skills used:** general-purpose, code-reviewer.

### Phase 3 — Sweep one package at a time

Recommended order (smallest to largest, leaf to root):

1. `domain/envelope.go` — only `HasExpired`. Add a `Clock`-taking
   method overload OR move the check to the route runner that
   already has a clock.

   **Status:** Resolved 2026-05-04. Envelope expiry and TTL calculations now require an injected clock argument, removing direct wall-clock reads from `domain/envelope.go` without retaining the old no-arg API.

   **What landed:**

   - Updated clock-aware expiry APIs in [domain/envelope.go](domain/envelope.go).
   - Passed injected runtime clocks in [runtime/route_runner.go](runtime/route_runner.go) and [runtime/outbox_drainer_retry.go](runtime/outbox_drainer_retry.go).
   - Updated remaining TTL call sites in [adapters/amqp/transport/amqp091/headers.go](adapters/amqp/transport/amqp091/headers.go), [adapters/azure/transport/servicebus/sender.go](adapters/azure/transport/servicebus/sender.go), [adapters/mqtt/transport/paho/headers.go](adapters/mqtt/transport/paho/headers.go), and [adapters/mqtt/transport/paho/sender.go](adapters/mqtt/transport/paho/sender.go).
   - Updated long-running test documentation in [tests/longrunning/uc27_failure_recovery_test.go](tests/longrunning/uc27_failure_recovery_test.go).

   **Tests added:**

   - Updated [domain/envelope_test.go](domain/envelope_test.go) to use a deterministic fake clock for expiry and TTL assertions.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** golang-pro, code-reviewer.
2. `circuitbreaker/breaker.go` — already uses time.Now in
   `transitionTo` and `consecutiveFailures` updates. Add a
   `WithBreakerClock` option.

   **Status:** Resolved 2026-05-04. Circuit breaker timestamps and reset-timeout checks now use an injected breaker clock, with `clock.System` as the default implementation.

   **What landed:**

   - Added `WithBreakerClock` and routed breaker elapsed-time and timestamp reads through the injected clock in [circuitbreaker/breaker.go](circuitbreaker/breaker.go).

   **Tests added:**

   - Added deterministic fake-clock coverage for breaker failure timestamps, retry-after calculations, and open-to-half-open timing in [circuitbreaker/breaker_test.go](circuitbreaker/breaker_test.go).

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - Processor-level circuit breaker construction still uses the default breaker clock; a later package sweep can thread runtime clock injection into [processors/circuitbreaker/processor.go](processors/circuitbreaker/processor.go).

   **Agents/Skills used:** general-purpose, code-reviewer.
3. `bridge/supervisor.go` — single `time.Now()` for swap timestamps.

   **Status:** Resolved 2026-05-04. Supervisor swap duration measurement now uses an injected `clock.Clock`, defaulting to `clock.System`, so tests can deterministically drive elapsed reconfiguration time without direct wall-clock reads.

   **What landed:**

   - Added `clock.Clock` injection and `WithSupervisorClock` to [bridge/supervisor.go](bridge/supervisor.go).
   - Replaced the direct `time.Now()` / `time.Since()` swap-duration measurement with the supervisor clock.

   **Tests added:**

   - Added deterministic fake-clock coverage for swap callback duration in [bridge/supervisor_test.go](bridge/supervisor_test.go).

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
4. `runtime/credential_resolver.go` — TTL cache; needs clock for
   expiry checks. ~3 calls.
5. `runtime/credentials_poll.go` — RNG seed; replace
   `time.Now().UnixNano()` with `clk.Now().UnixNano()`.
6. `runtime/dlq_router.go` — `FailedAt` field on DLQ entries.
7. `runtime/route_runner.go` + helpers — backoff math.
8. `runtime/session_manager*.go` — lease renewal scheduling.
9. `runtime/outbox_drainer_loop.go` — drain cycle timestamps.
10. `runtime/instrumented*.go` — metric timestamps.
11. `httpapi/{admin,admin_dlq,config_txn,server}.go` — request-time
    audit log timestamps. May not need full clock injection if
    they only stamp wall-clock for human-readable output.
12. `adapters/*/transport/*` — message timestamps, deadlines. Each
    transport adapter is one PR of work.
13. `adapters/native/credentials/file/repository.go` — file mtime
    polling.
14. `adapters/native/store/sqliteoutbox/store.go` — record
    timestamps.

Per package: build + test green at each step.

### Phase 4 — Address the `loader_test.go` TODO

Now that the underlying production code uses `l.clk.NewTicker`,
rewrite the test to:

```go
fc := clocktest.New()
loader := ddbconfig.NewLoader(client,
    ddbconfig.WithTableName(tableName),
    ddbconfig.WithBridgeID("test-bridge"),
    ddbconfig.WithPollInterval(100*time.Millisecond),
    ddbconfig.WithClock(fc),
)
// ...
ch, err := loader.Watch(ctx)
// Wait for the goroutine to register its ticker before advancing.
require.Eventually(t, func() bool { return fc.TickerCount() == 1 },
    time.Second, 5*time.Millisecond)
// Fire 5 poll cycles.
fc.Advance(500 * time.Millisecond)
// Brief real-time yield so the goroutine can drain its tick queue
// before we inspect the channel — this is shorter than 500ms because
// the work between ticks is just a DDB call, not real waiting.
require.Eventually(t, func() bool { return /* some side-effect counter */ },
    time.Second, 5*time.Millisecond)
// Now safely assert no emission.
select {
case got := <-ch:
    require.Nil(t, got)
default:
}
```

Drop the `//nolint:forbidigo` and the `time.Sleep(500ms)` once the
fake-clock variant works.

### Phase 5 — Enable `forbidigo` in `.golangci.yml`

```yaml
linters:
  enable:
    - forbidigo   # was commented out
```

Drop the file-level exclusions for production files (keep the
test-file and `domain/clock/real.go` / `clocktest/` exemptions).

`make lint` should pass with no `forbidigo` violations remaining.
A trial `time.Now()` in a production file should fail the gate.

## Cost estimate

- ≈ 40 production files × 30-60 min each = **3–5 dedicated days**
- The mechanical work is consistent (struct field + option +
  call-site rewrite) but each call site needs a judgement on where
  the clock should live.

## Risks

- **Tests break in surprising ways.** Some tests assert "an event
  happens within X ms" using real clocks. Switching production code
  to fake clock without converting the test exposes the implicit
  dependency. Convert per-package: production → fake-aware tests in
  the same commit.
- **clock.System is the default.** Forgetting to set the default
  will produce nil-deref panics. Compile-time guard:
  `var _ clock.Clock = (*clock.realClock)(nil)` already exists; add
  a constructor invariant test that asserts the default.
- **Performance.** A pointer dispatch to `clk.Now()` is slightly
  more expensive than `time.Now()` (interface call). Negligible in
  practice; benchmark hot paths only if profiling shows it.

## Acceptance

- `forbidigo` is enabled in `.golangci.yml`.
- Production-file exclusions are removed (only `domain/clock/real.go`,
  `domain/clock/clocktest/`, and `_test.go` remain).
- `make lint` passes; a trial `time.Now()` injection in a production
  file fails the gate.
- `adapters/aws/config/dynamodb/loader_test.go` no longer needs the
  `//nolint:forbidigo` escape hatch on its sleep (or the sleep is
  gone entirely).
- `audit/timing-allowlist.txt` shrinks (most "needs fake clock"
  entries become unjustified).

## Related

- Original plan: `FINAL_DDD_HEX_CLEAN_FIX_PLAN.md` § FIX-005.
- Companion follow-ups: `FIX-TODO-error-wrapping.md`,
  `FIX-TODO-return-types.md`, `FIX-TODO-test-quality.md`.
- Existing infrastructure: `domain/clock/`, `domain/clock/clocktest/`.
