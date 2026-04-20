# Timing Audit

Every production-code use of `time.Sleep`, `time.After`, `time.NewTicker`,
`time.NewTimer`, and hardcoded `context.WithTimeout` is classified below.

## Classification key

| Class | Meaning | Action |
|---|---|---|
| **REPLACEABLE** | Sleep substitutes for a missing signal | Add the missing event/channel/hook; delete the sleep |
| **DEADLINE** | Upper bound on a bounded operation | Derive from operational config; never hardcode |
| **ESSENTIAL** | Genuine waiting (backoff, protocol cadence) | Keep, make cancellable via `select { ctx; clk.After }`, expose in config, route through `Clock` |

## Production hotspots

| File | Line(s) | Current value | Class | Rationale / action |
|---|---|---|---|---|
| `runtime/dlq_router.go` | 227-231 | `Sleep(500ms)` × 3 retries | **ESSENTIAL** | Real backoff against opaque store. Make cancellable + configurable via `DLQRouterConfig.WriteRetryBackoff`. |
| `adapters/http/transport/forwarder.go` | 99-100 | `[100ms, 200ms]` retries, `maxRetries=2` | **ESSENTIAL** | Real backoff against remote peer. Expose via `ForwarderConfig.RetryBackoff`. |
| `adapters/http/transport/forwarder.go` | 42 | `IdleConnTimeout=90s` | **DEADLINE** | Derive from cluster heartbeat interval; expose in `ForwarderConfig`. |
| `runtime/route_locator.go` | 13-15 | `cacheTTL=2s, maxFailures=3, cooldown=5s` | **ESSENTIAL** | Operator-tunable cache / circuit. Lift to `RouteLocatorConfig`. |
| `adapters/aws/transport/sqs/receiver.go` | 57 | `30s` init `WithTimeout` | **DEADLINE** | Derive from `ReceiverConfig.ConnectTimeout`. |
| `adapters/aws/transport/sqs/receiver.go` | 164-166 | `pollBackoff 1s..30s` | **ESSENTIAL** | Real backoff on `ReceiveMessage` failure. Lift to `BackoffPolicy` field. |
| `adapters/amqp/transport/amqp10/session.go` | 481 | `NewTicker(5s)` monitor | **REPLACEABLE** | go-amqp exposes `Session.Done()` / `Conn.Done()`. Subscribe; ticker becomes 30s sanity fallback. |
| `adapters/amqp/transport/amqp10/session.go` | 575 | `time.After(sleepDur)` reconnect | **ESSENTIAL** | Real backoff. Keep, make cancellable, expose policy. |
| `adapters/amqp/transport/amqp10/session.go` | 245 | `5s` close old session | **DEADLINE** | Derive from drain timeout. |
| `adapters/amqp/transport/amqp091/session.go` | 569 | `time.After(250ms)` conn==nil | **REPLACEABLE** | Replace with `select` on `connEstablished` channel. |
| `adapters/amqp/transport/amqp091/session.go` | 642 | `time.After(sleepDur)` reconnect | **ESSENTIAL** | Same pattern as amqp10. |
| `runtime/route_runner.go` | 173 | `5s WithTimeout` panic-retry | **DEADLINE** | Derive from `RuntimeConfig.ShutdownTimeout`. |
| `runtime/route_runner.go` | 193 | `10s WithTimeout` receiver Close | **DEADLINE** | Derive from drain timeout. |
| `runtime/outbox_drainer.go` | 189-191 | `5s` stale-token backoff floor | **ESSENTIAL** | Real backoff. Lift to `StaleTokenMinBackoff` config field. |
| `runtime/outbox_drainer.go` | 262 | `+5s` batch timeout slack | **DEADLINE** | Derive as `1.5 × SendTimeout` (clamped to drain timeout). |
| `runtime/session_manager.go` | 372 | `5s WithTimeout` Release | **DEADLINE** | Derive from `StepDownGrace`. |
| `runtime/bridge.go` | 479 | `5s WithTimeout` session Close | **DEADLINE** | Derive from `min(drainTimeout, 5s)`. |
| `runtime/bridge.go` | 492 | `5s WithTimeout` metrics Flush | **DEADLINE** | Derive from `min(drainTimeout/2, 5s)`. |
| `bridge/builder.go` | 634 | `15s` staleClaimBuffer | **DEADLINE** | Replace with `2 × max(StepDownGrace)`. |
| `processors/tenant/processor.go` | 77 | `5s WithTimeout` decrement | **DEADLINE** | Propagate parent ctx; tiny fallback only when ctx is already done. |
| `adapters/aws/metrics/cloudwatch/exporter.go` | 159, 167 | `30s` flush RPC timeout | **DEADLINE** | Derive from `FlushInterval / 2`. |
| `adapters/aws/cluster/ecs/resolver.go` | 35 | `5s` HTTP client timeout | **DEADLINE** | Lift to constructor option. |
| `adapters/azure/transport/servicebus/delivery.go` | 64-66 | `1s` auto-extend floor | **ESSENTIAL** | Protocol-mandated cadence. Lift floor to config. |
| `adapters/aws/transport/sqs/delivery.go` | 289 | `visibility/2` auto-extend | **ESSENTIAL** | Already derived from protocol. Route through `Clock`. |
| `adapters/mqtt/transport/paho/sender.go` | 98 | `500ms` throttle hint | **ESSENTIAL** | Lift to `SenderOptions`. |
| `adapters/mqtt/transport/paho/session.go` | 151, 292 | `30s` reconcile / connect fallback | **REPLACEABLE** + **DEADLINE** | Emit completion event; fallback becomes sanity ceiling from session config. |
| `adapters/aws/config/dynamodb/loader.go` | 141 | poll ticker (default 30s) | **ESSENTIAL** → **REPLACEABLE** | DDB Streams provides push. Demote poll to fallback. |
| `adapters/native/config/file/watcher.go` | 19, 205, 238 | `100ms` debounce, `30s` poll | **REPLACEABLE** → **ESSENTIAL** fallback | fsnotify for push; poll on unsupported platforms. |

## Test sleep classification (aggregate)

| Category | Baseline | After Phase 1 | After Phase 2 | After Tier 1 | After Tier 2 | Description |
|---|---|---|---|---|---|---|
| SYNC_SLEEP | ~280 | ~260 | ~100 | ~70 | ~48 | "sleep then assert" — most replaced by readiness APIs |
| STARTUP_SLEEP | ~50 | ~45 | ~20 | ~20 | ~15 | "let goroutine start" — replaced by `Started()` / `LeaseStateChanged()` |
| NEGATIVE_ASSERT_WINDOW | ~30 | ~25 | ~15 | ~12 | ~12 | "sleep then assert nothing happened" — `wait.Silent` |
| FIXED_BACKOFF_LOOP | ~20 | ~8 | ~3 | ~3 | ~3 | poll loops in test helpers — `wait.Until` / `RequireReceive` |
| OTHER | ~6 | ~6 | ~110 | ~110 | ~110 | intentional stress/race timers, debounce choreography, clock test companions, simulated work |
| **Total** | **~386** | **~344** | **~248** | **~215** | **~188** | All remaining 188 tracked in `audit/test-timing-allowlist.txt` with classification comments |

**Phase 2 changes (per slice):**
- **Slice 1 (MQTT):** 51 → 14 sleeps; 37 removed via `waitSubActive`, `<-recv.Started()`, NEGATIVE select
- **Slice 2 (AMQP 1.0):** 9 → 5 sleeps; `s.clk.NewTicker` for monitor loop, `Clock` on `ReceiverConfig`, `LinkCloseTimeout` lifted
- **Slice 3 (AMQP 0.9.1):** 9 → 3 sleeps; `wait.Until` on subscriber/connection state
- **Slice 4 (SQS):** 16 → 8 sleeps; `clocktest.Fake.Advance` for auto-extend, NEGATIVE select for no-extend
- **Slice 5 (ASB):** 11 → 11 sleeps (annotated; `cfg.Clock.After` in production receiver)
- **Slice 6 (HTTP):** 10 → 2 sleeps; `<-recv.Started()`, `ClientCount()` polling, `AdminOperationTimeout` lifted
- **Slice 7 (file watcher):** 6 → 4 sleeps; `clocktest.Fake.Advance` for poll loop
- **Slice 8 (DDB loader):** `l.clk.NewTicker` for pollLoop; 1 sleep annotated
- **Slice 9 (CloudWatch):** `Clock` on Exporter, `FlushRPCTimeout` capped; no test sleeps
- **Slice 10 (bridge/runtime/misc):** ~85 sleeps annotated with classification comments; production: `Clock` on strategies, timeout lifts across 7 files

**Phase 2 production Clock wiring:**
All raw `time.NewTicker`, `time.NewTimer`, `time.After` calls in adapter/runtime/bridge production code now route through `domain/clock.Clock`. Components with new `WithClock` options: SSE sender, HTTP forwarder, file watcher, CloudWatch exporter, DebouncedStrategy, WindowedStrategy. Components completing partial migration: DDB loader pollLoop, AMQP 1.0 monitor ticker + receiver backoff, ASB receiver backoff.

Most `Reconcile() + Sleep(200ms)` patterns in `tests/integration/` have been replaced with
`wait.Until` polling `sess.Health().ServiceLevel == Full`, and several MQTT collector
initializations use the same approach instead of a fixed delay.

**Tier 1 mechanical determinism wins (248 → 215, 33 additional sleeps removed):**
- **Slice A (ASB delivery):** 7 wall-clock sleeps replaced with `clocktest.Fake.Advance` for auto-renew tests; test runtime dropped from ~25s to ~1s
- **Slice B (bridge reconfig strategy):** 5 debounce-choreography sleeps replaced with `clocktest.Fake.Advance` now that strategies accept `Clock`; runtime dropped from ~1.5s to ~50ms
- **Slice C (runtime):** 5 SYNC sleeps replaced with `waitFor(t, ...)` polling on observable counters/flags
- **Slice D (longrunning + integration):** ~16 post-Reconcile sleeps replaced with `waitSubReady` polling `ServiceLevelFull`; `newMQTTCollector` helpers in longrunning now use health-based readiness instead of fixed delays

**Tier 2 changes (readiness APIs + sweep, 215 → 188, 27 additional sleeps removed):**
- **Slice 1 (per-route readiness):** `RouteHealth.Ready` + `RouteHealth.InFlight` in `DeepHealth`; `RouteRunner.Started()` channel; `Runtime.WaitRouteReady(ctx, routeID)`
- **Slice 2 (outbox idle):** `OutboxDrainer.IdleSince()` + `WaitIdle(ctx, minQuiet)` with channel-swap mechanism
- **Slice 3 (quiescence):** `RouteRunner.InFlight()` atomic counter; `Runtime.WaitQuiescent(ctx, QuiescenceOptions)` for zero in-flight + stability window
- **Slice 4 (lease events):** `SessionManager.LeaseStateChanged() <-chan LeaseStateEvent` with 6 states (Acquired/Renewed/Lost/SteppedDown/Released/None)
- **Slice 5 (per-topic health):** `SessionHealth.ActiveTopics []string` + `HasTopic(topic)` helper
- **Slice 6 (SSE client):** `SSESender.WaitClientConnected(ctx, n)` polling `ClientCount()`
- **Slice 7 (SQS/ASB Started):** wired existing `<-recv.Started()` in 2 integration tests
- **Slice 8 (file watcher):** `Watcher.Started()` channel + `LastApplied() time.Time` atomic timestamp
- **Sweep:** 15 longrunning `WaitQuiescent` replacements, 5 session-manager `LeaseStateChanged` replacements, 2 watcher `Started()` replacements, 2 SSE `WaitClientConnected` replacements, 3 integration `WaitQuiescent/WaitRouteReady` replacements

## Readiness API inventory

| Component | API | Signal type | When signaled | File |
|---|---|---|---|---|
| `ports.Receiver` (optional) | `Started() <-chan struct{}` | Channel close | Receive loop live | `ports/transport.go` |
| `ports.Session` | `Health(ctx) SessionHealth` | Struct poll | Anytime | `ports/transport.go` |
| `ports.Session` | `Events() <-chan SessionEvent` | Event push | Connect/disconnect/reconcile | `ports/transport.go` |
| `ports.SessionHealth` | `ActiveTopics []string` / `HasTopic()` | Struct field | From session `Health()` | `ports/transport.go` |
| `runtime.Runtime` | `DeepHealth(ctx) DeepHealth` | Struct poll | Anytime | `runtime/bridge.go` |
| `runtime.Runtime` | `WaitRouteReady(ctx, routeID) error` | Blocking wait | Route runner + receiver started | `runtime/bridge.go` |
| `runtime.Runtime` | `WaitQuiescent(ctx, opts) error` | Blocking wait | All routes at zero in-flight for `MinQuiet` | `runtime/bridge.go` |
| `runtime.Runtime` | `ReadinessLevel(ctx) ReadinessLevel` | Snapshot enum | Down/Live/Running/Connected/Subscribed/Full | `runtime/bridge.go` |
| `runtime.Runtime` | `AtLeast(ctx, want) bool` | Snapshot bool | Has reached requested level | `runtime/bridge.go` |
| HTTP `/api/v1/monitor/ready?level=` | level= query param | HTTP 200/503 | live, running, connected, subscribed, full | `httpapi/monitor.go` |
| HTTP `/api/v1/monitor/deephealth` | JSON body | Per-session `active_topics`, per-route `ready` + `in_flight`, current `level` | exposed in response | `httpapi/monitor.go` |
| `runtime.RouteRunner` | `Started() <-chan struct{}` | Channel close | `Run()` entered | `runtime/route_runner.go` |
| `runtime.RouteRunner` | `InFlight() int64` | Atomic counter | Per delivery enter/exit | `runtime/route_runner.go` |
| `runtime.RouteHealth` | `Ready bool` / `InFlight int` | Struct fields | From `DeepHealth()` | `runtime/bridge.go` |
| `runtime.OutboxDrainer` | `IdleSince() (time.Time, bool)` | Atomic timestamp | Pending → empty transition | `runtime/outbox_drainer.go` |
| `runtime.OutboxDrainer` | `WaitIdle(ctx, minQuiet) error` | Blocking wait | Continuously idle for `minQuiet` | `runtime/outbox_drainer.go` |
| `runtime.SessionManager` | `LeaseStateChanged() <-chan LeaseStateEvent` | Event push | Acquire/renew/release/stepdown/loss | `runtime/session_manager.go` |
| `paho.Receiver` | `Started() <-chan struct{}` | Channel close | Handler registered on router | `adapters/mqtt/transport/paho/receiver.go` |
| `paho.Session` | `Health()` / `Events()` | Poll + event | Connect/reconcile/disconnect | `adapters/mqtt/transport/paho/session.go` |
| `http.SSESender` | `WaitClientConnected(ctx, n) error` | Blocking wait | N clients connected | `adapters/http/transport/sender_sse.go` |
| `file.Watcher` | `Started() <-chan struct{}` | Channel close | fsnotify registered | `adapters/native/config/file/watcher.go` |
| `file.Watcher` | `LastApplied() time.Time` | Atomic timestamp | Config successfully applied | `adapters/native/config/file/watcher.go` |

## Integration-test content verification

The `testutil/testcontent` package provides deterministic message content verification.
Every test message is tagged with a unique TID (test-message-ID) in both a header
(`x-bridge.test-msg-id`) and a JSON payload field (`_tid`).

Assertion helpers compare sent vs received sets by TID:

- `AssertReceivedSet` — set(sent.TID) == set(received.TID); reports missing and extra
- `AssertContentMatches` — set equality + per-TID payload/header comparison
- `AssertNoDuplicates` — each TID appears at most once
- `AssertOrdered` — received TIDs appear in the same order as sent

The `ExtractTID` function tries the header first, then falls back to the JSON `_tid` field,
so it works even when transport headers are stripped (e.g. SQS → body-only polling).

**MQTT Envelope.ID round-trip**: `PublishFromEnvelope` now includes the `Envelope.ID` as a
`mqtt.message-id` user property. `EnvelopeFromPublish` recovers it in priority order:
1. `mqtt.message-id` user property
2. `x-bridge.correlation-id` from CorrelationData
3. Random fallback

This enables `countUnique()` to work correctly on MQTT collectors for all throughput,
resilience, and backpressure tests.

## Irreducible ESSENTIAL set (after Phase 1)

These are the only legitimate timing uses that survive event-signal elimination:

1. **Backoff against opaque dependencies**: DLQ store writes, AMQP/SQS/ASB receive failures, AMQP reconnect, HTTP forwarder retries, route locator failure cooldown
2. **Protocol-mandated cadence**: SQS visibility extension, ASB lock renewal, lease renewal, MQTT keepalive
3. **Operator-controlled rate limits / cache TTLs**: route locator cache TTL, outbox drainer adaptive poll, file watcher fallback poll
