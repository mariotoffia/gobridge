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

   **Status:** Resolved 2026-05-04. Credential resolver cache expiry, eviction, and stats now use an injected `clock.Clock`, defaulting to `clock.System`, so TTL behavior can be tested deterministically without direct wall-clock reads.

   **What landed:**

   - Added `clock.Clock` injection and `WithCredentialClock` in [runtime/credential_resolver.go](runtime/credential_resolver.go).
   - Replaced all direct cache `time.Now()` reads in [runtime/credential_resolver.go](runtime/credential_resolver.go) with the resolver clock.
   - Converted cache-expiry tests in [runtime/credential_resolver_test.go](runtime/credential_resolver_test.go) from `time.Sleep` to `clocktest.Fake` advancement.

   **Tests added:**

   - Updated `TestCredentialResolver_CacheExpiry`, `TestCredentialResolver_CacheEvictsExpired`, and `TestCredentialResolver_CacheStats_AccurateExpiredCount` to use deterministic fake clocks.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - Removed sleep-driven race windows from credential resolver cache-expiry tests.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
5. `runtime/credentials_poll.go` — RNG seed; replace
   `time.Now().UnixNano()` with `clk.Now().UnixNano()`.

   **Status:** Resolved 2026-05-04. Poll-based credential jitter seeding now uses the configured clock after options are applied, so fake clocks determine the RNG seed without direct wall-clock reads.

   **What landed:**

   - Updated [runtime/credentials_poll.go](runtime/credentials_poll.go) to seed jitter RNG from `w.clk.Now().UnixNano()` after `WithPollClock` options are applied.
   - Added deterministic injected-clock seed coverage in [runtime/credentials_poll_test.go](runtime/credentials_poll_test.go).

   **Tests added:**

   - Added `TestPollBasedWrapper_JitterSeedUsesInjectedClock`.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
6. `runtime/dlq_router.go` — `FailedAt` field on DLQ entries.

   **Status:** Resolved 2026-05-04. DLQ entry failure timestamps now use the router's injected `clock.Clock`, removing the direct wall-clock read from `runtime/dlq_router.go`.

   **What landed:**

   - Replaced `time.Now()` with `r.clk.Now()` for `domain.DLQEntry.FailedAt` in [runtime/dlq_router.go](runtime/dlq_router.go).
   - Updated [runtime/dlq_router_test.go](runtime/dlq_router_test.go) to construct the router with `clocktest.Fake` and assert the persisted `FailedAt` matches the injected instant.

   **Tests added:**

   - Updated `TestDLQRouter_Route_AllFieldsPopulated` with deterministic fake-clock coverage for `FailedAt`.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
7. `runtime/route_runner.go` + helpers — backoff math.

   **Status:** Resolved 2026-05-04. Route runner delivery latency and shared-outbox record creation timestamps now use the runner's injected `clock.Clock`, removing direct wall-clock reads from route-runner timing paths.

   **What landed:**

   - Replaced route-runner delivery latency timing in [runtime/route_runner.go](runtime/route_runner.go) with `r.clk.Now()` / `r.clk.Since()`.
   - Replaced shared-outbox record `CreatedAt` timestamps in [runtime/route_runner_helpers.go](runtime/route_runner_helpers.go) with `r.clk.Now()`.
   - Added fake-clock coverage in [runtime/route_runner_clock_test.go](runtime/route_runner_clock_test.go) for latency metrics and outbox `CreatedAt` timestamps.

   **Tests added:**

   - Added `TestRouteRunner_E2ELatencyUsesInjectedClock`.
   - Added `TestRouteRunner_SharedOutboxCreatedAtUsesInjectedClock`.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
8. `runtime/session_manager*.go` — lease renewal scheduling.

   **Status:** Resolved 2026-05-04. Session manager lease-audit timestamps and lease acquire/renew latency measurements now use the manager's injected `clock.Clock`, removing direct wall-clock reads from the production session-manager timing path.

   **What landed:**

   - Replaced the lease audit timestamp in [runtime/session_manager.go](runtime/session_manager.go) with `m.clk.Now().UTC()`.
   - Replaced lease acquire and renew latency timing in [runtime/session_manager_lease.go](runtime/session_manager_lease.go) with `m.clk.Now()` / `m.clk.Since()`.

   **Tests added:**

   - none — existing fake-clock renewal coverage in [runtime/session_manager_clock_test.go](runtime/session_manager_clock_test.go) continues to cover injected-clock scheduling.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - none.

   **Follow-ups (not blockers; logged for future passes):**

   - [runtime/session_manager_clock_test.go](runtime/session_manager_clock_test.go) still uses small real-time waits as test-goroutine synchronization guards; these are outside the production forbidigo gate but can be replaced with channel-based synchronization in a later test cleanup.

   **Agents/Skills used:** general-purpose, code-reviewer.
9. `runtime/outbox_drainer_loop.go` — drain cycle timestamps.

   **Status:** Resolved 2026-05-04. Outbox drain-batch latency measurement and debug duration logging now use the drainer's injected `clock.Clock`, removing direct wall-clock reads from the drain-cycle timing path.

   **What landed:**

   - Replaced drain-batch start and elapsed-time calculations in [runtime/outbox_drainer_loop.go](runtime/outbox_drainer_loop.go) with `d.clk.Now()` / `d.clk.Since()`.
   - Added deterministic fake-clock metric coverage in [runtime/outbox_drainer_clock_test.go](runtime/outbox_drainer_clock_test.go) for `MetricOutboxDrainLatency`.
   - Replaced sleep-based synchronization in [runtime/outbox_drainer_clock_test.go](runtime/outbox_drainer_clock_test.go) with fake-timer registration polling that yields via `runtime.Gosched`.

   **Tests added:**

   - Added `TestOutboxDrainer_DrainLatencyUsesInjectedClock`.

   **Pre-existing issues fixed in touched files (per audit instruction):**

   - Removed real `time.Sleep` synchronization from existing outbox drainer fake-clock tests so the timing audit remains clean when the file changes.

   **Follow-ups (not blockers; logged for future passes):**

   - none.

   **Agents/Skills used:** general-purpose, code-reviewer.
10. `runtime/instrumented*.go` — metric timestamps.

    **Status:** Resolved 2026-05-04. Instrumented runtime store and transport latency metrics now use injected `clock.Clock` instances, with constructor signatures requiring a clock so no legacy no-clock API remains.

    **What landed:**

    - Added injected clock fields to [runtime/instrumented.go](runtime/instrumented.go) for lease and outbox latency timers.
    - Added injected clock fields to [runtime/instrumented_transport.go](runtime/instrumented_transport.go) for sender, receiver, and delivery ack latency timers.
    - Updated instrumentation constructor call sites in [runtime/instrumented_test.go](runtime/instrumented_test.go) and [runtime/instrumented_transport_test.go](runtime/instrumented_transport_test.go) to pass clocks explicitly.

    **Tests added:**

    - Added deterministic fake-clock coverage for lease acquire/renew, outbox persist, sender send, receiver run, and delivery ack latency metrics in [runtime/instrumented_test.go](runtime/instrumented_test.go) and [runtime/instrumented_transport_test.go](runtime/instrumented_transport_test.go).

    **Pre-existing issues fixed in touched files (per audit instruction):**

    - none.

    **Follow-ups (not blockers; logged for future passes):**

    - none.

    **Agents/Skills used:** general-purpose, code-reviewer.
11. `httpapi/{admin,admin_dlq,config_txn,server}.go` — request-time
    audit log timestamps. May not need full clock injection if
    they only stamp wall-clock for human-readable output.

    **Status:** Resolved 2026-05-04. HTTP API audit timestamps, DLQ purge cutoffs, request-duration logging, and config transaction timing now use the server's injected `clock.Clock`, removing direct wall-clock reads from the target production files without retaining legacy compatibility shims.

    **What landed:**

    - Added server-level `clock.Clock` injection and `WithClock` in [httpapi/server.go](httpapi/server.go), defaulting to `clock.System`.
    - Routed audit, DLQ purge, request logging, and config transaction timestamps/timers through the injected clock in [httpapi/admin.go](httpapi/admin.go), [httpapi/admin_dlq.go](httpapi/admin_dlq.go), [httpapi/config_txn.go](httpapi/config_txn.go), and [httpapi/server.go](httpapi/server.go).
    - Updated deterministic fake-clock coverage in [httpapi/admin_config_test.go](httpapi/admin_config_test.go), [httpapi/admin_config_version_test.go](httpapi/admin_config_version_test.go), and [httpapi/server_bugfix_test.go](httpapi/server_bugfix_test.go).

    **Tests added:**

    - Updated existing HTTP API config and server tests to assert injected-clock behavior for audit timestamps, purge cutoffs, transaction creation, and auto-timeout.

    **Pre-existing issues fixed in touched files (per audit instruction):**

    - Guarded config transaction timeout cleanup by transaction ID so a stale timer goroutine cannot roll back a newer transaction.

    **Follow-ups (not blockers; logged for future passes):**

    - none.

    **Agents/Skills used:** general-purpose, code-reviewer.
12. `adapters/*/transport/*` — message timestamps, deadlines. Each
    transport adapter is one PR of work.

    **Status:** Resolved 2026-05-04. Transport adapter message timestamps, latency measurements, deadlines, and TTL calculations now use injected `clock.Clock` instances across AMQP 0-9-1, AMQP 1.0, SQS, Azure Service Bus, HTTP, and MQTT without retaining legacy no-clock compatibility APIs.

    **What landed:**

    - Threaded clocks through AMQP 0-9-1 delivery, headers, receiver, sender, session, and config paths in [adapters/amqp/transport/amqp091](adapters/amqp/transport/amqp091).
    - Threaded clocks through AMQP 1.0 delivery, receiver, sender, session, and config paths in [adapters/amqp/transport/amqp10](adapters/amqp/transport/amqp10).
    - Threaded clocks through SQS delivery, receiver, sender, and config paths in [adapters/aws/transport/sqs](adapters/aws/transport/sqs).
    - Threaded clocks through Azure Service Bus delivery, receiver, sender, and config paths in [adapters/azure/transport/servicebus](adapters/azure/transport/servicebus).
    - Threaded clocks through HTTP factory, receiver, and SSE sender paths in [adapters/http/transport](adapters/http/transport).
    - Threaded clocks through MQTT/Paho headers, receiver, sender, session, health, lifecycle, and reconcile paths in [adapters/mqtt/transport/paho](adapters/mqtt/transport/paho).

    **Tests added:**

    - none — updated existing tests for new clock-threaded helper signatures.

    **Pre-existing issues fixed in touched files (per audit instruction):**

    - none.

    **Follow-ups (not blockers; logged for future passes):**

    - Consider capturing a single `now := clk.Now()` in Azure Service Bus `newDelivery` lock-remaining math for stricter fake-clock determinism.

    **Agents/Skills used:** golang-pro, code-reviewer.
13. `adapters/native/credentials/file/repository.go` — file mtime
    polling.

    **Status:** Resolved 2026-05-04. File credential create/update timestamps now use an injected `clock.Clock`, with deterministic fake-clock test coverage and no production `time.Now()` call remaining in the file repository.

    **What landed:**

    - Added `clock.Clock` injection to [adapters/native/credentials/file/repository.go](adapters/native/credentials/file/repository.go) and replaced create/update timestamp reads with `r.clk.Now()`.
    - Updated [adapters/native/credentials/file/repository_test.go](adapters/native/credentials/file/repository_test.go) to assert create/update timestamps with `clocktest.Fake`.

    **Tests added:**

    - Updated existing file repository timestamp tests to use deterministic fake-clock assertions.

    **Pre-existing issues fixed in touched files (per audit instruction):**

    - Removed wall-clock sleep from the update timestamp preservation test.

    **Follow-ups (not blockers; logged for future passes):**

    - none.

    **Agents/Skills used:** golang-pro, code-reviewer.
14. `adapters/native/store/sqliteoutbox/store.go` — record
    timestamps. - DONE

    **Status:** Resolved 2026-05-04. Clock injection already present from Phase 2 sweep. `Store.clk clock.Clock` field, `WithClock` option, and `s.clk.Now()` usage in `Persist` (createdAt) and `Complete` (completedAt) confirmed; `TestWithClockControlsCreatedAt` test passes; no production `time.Now()` calls remain.

    **What landed:**

    - No new changes required — [adapters/native/store/sqliteoutbox/store.go](adapters/native/store/sqliteoutbox/store.go) and [adapters/native/store/sqliteoutbox/store_test.go](adapters/native/store/sqliteoutbox/store_test.go) already fully migrated as part of T002.

    **Tests added:**

    - none — `TestWithClockControlsCreatedAt` already present.

    **Pre-existing issues fixed in touched files (per audit instruction):**

    - none.

    **Follow-ups (not blockers; logged for future passes):**

    - none.

    **Agents/Skills used:** golang-pro, code-reviewer.

Per package: build + test green at each step. - DONE

**Status:** Resolved 2026-05-04. All packages touched during the clock-injection sweep (T003–T016) verified green: domain, circuitbreaker, bridge, runtime, adapters/http/transport, httpapi, adapters/amqp/transport/amqp091, adapters/amqp/transport/amqp10, adapters/aws/transport/sqs, adapters/azure/transport/servicebus, adapters/mqtt/transport/paho, adapters/native/credentials/file, adapters/native/store/sqliteoutbox, adapters/native/store/memoryoutbox, adapters/native/store/memorylease, adapters/aws/store/dynamodblease, adapters/aws/store/dynamodboutbox — all build clean and unit tests pass under `go test -short -race -timeout 120s ./...`.

**What landed:**

- No code changes — verification checkpoint only.

**Tests added:**

- none.

**Pre-existing issues fixed in touched files (per audit instruction):**

- none.

**Follow-ups (not blockers; logged for future passes):**

- none.

**Agents/Skills used:** code-reviewer.

### Phase 4 — Address the `loader_test.go` TODO - DONE

**Status:** Resolved 2026-05-04. `TestWatchNoDuplicates` in `adapters/aws/config/dynamodb/loader_test.go` rewired to inject `clocktest.Fake` via `ddbconfig.WithClock`; the 500ms `time.Sleep` and its `//nolint:forbidigo` annotation are removed. The goroutine registration is confirmed via a `fc.TickerCount()` spin-wait, then `fc.Advance(500ms)` fires five poll cycles deterministically.

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

**What landed:**

- Rewrote `TestWatchNoDuplicates` in [adapters/aws/config/dynamodb/loader_test.go](adapters/aws/config/dynamodb/loader_test.go) to inject `clocktest.Fake` via `ddbconfig.WithClock`; replaced `time.Sleep(500ms)` with `fc.Advance(500ms)` and a `fc.TickerCount()` spin-wait for goroutine registration; dropped `//nolint:forbidigo` annotation.

**Tests added:**

- Updated `TestWatchNoDuplicates` to use deterministic fake-clock advancement.

**Pre-existing issues fixed in touched files (per audit instruction):**

- Removed the TODO comment and forbidigo nolint exemption.

**Follow-ups (not blockers; logged for future passes):**

- none.

**Agents/Skills used:** code-reviewer.

### Phase 5 — Enable `forbidigo` in `.golangci.yml` — DONE

**Status:** Resolved 2026-05-04. `forbidigo` enabled in `.golangci.yml`; two
missed callers fixed (`batcher.go` and `outbox_depth_cache.go`); all production
code lint-clean; negative-test confirmed the rule fires on injected `time.Now`.

```yaml
linters:
  enable:
    - forbidigo   # was commented out
```

Drop the file-level exclusions for production files (keep the
test-file and `domain/clock/real.go` / `clocktest/` exemptions).

`make lint` should pass with no `forbidigo` violations remaining.
A trial `time.Now()` in a production file should fail the gate.

**What landed:**

- Enabled `forbidigo` in `.golangci.yml` (linters.enable block).
- Fixed `adapters/aws/metrics/cloudwatch/batcher.go`: added `clk clock.Clock`
  field to `batcher` struct; updated `newBatcher(namespace, defaultTags, maxSize, clk)`
  signature; replaced 2× `time.Now()` with `b.clk.Now()`; no backward-compat shim.
- Updated `adapters/aws/metrics/cloudwatch/exporter.go`: passes `e.config.Clock`
  to `newBatcher`.
- Updated `adapters/aws/metrics/cloudwatch/exporter_test.go`: 8 `newBatcher` calls
  updated to pass `clock.System`.
- Fixed `runtime/outbox_depth_cache.go`: replaced `now func() time.Time` with
  `clk clock.Clock`; updated `newOutboxDepthCache(ttl, clk)` signature; all
  `c.now()` → `c.clk.Now()`.
- Updated `runtime/route_runner.go`: moved depth cache creation after clock
  resolution; passes `clk` to `newOutboxDepthCache`.
- Added `ports/storetest/` and `testutil/` forbidigo exclusions (both are
  explicitly out-of-production-gate per Phase 1 harvest).
- Negative-test confirmed: injecting `func() time.Time { return time.Now() }`
  in `runtime/` causes golangci-lint to report `use of time.Now forbidden`.

**Tests added:**

- No new test files; existing tests pass with updated signatures.

**Pre-existing issues fixed in touched files (per audit instruction):**

- `outbox_depth_cache.go`: migrated from legacy `now func() time.Time` pattern
  to standard `clock.Clock` interface.

**Follow-ups (not blockers; logged for future passes):**

- none.

**Agents/Skills used:** forbidigo negative-test via manual golangci-lint run.

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
