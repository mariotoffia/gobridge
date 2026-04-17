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

| Category | Count | Description |
|---|---|---|
| SYNC_SLEEP | ~280 | "sleep then assert" — replaceable once production exposes event hooks |
| STARTUP_SLEEP | ~50 | "let goroutine start" — replaceable with `Started()` channel |
| NEGATIVE_ASSERT_WINDOW | ~30 | "sleep then assert nothing happened" — marker-event pattern |
| FIXED_BACKOFF_LOOP | ~20 | poll loops in test helpers — `wait.Until` / `RequireReceive` |
| OTHER | ~6 | intentional stress timers in fakes — route through `Clock` |

## Irreducible ESSENTIAL set (after Phase 1)

These are the only legitimate timing uses that survive event-signal elimination:

1. **Backoff against opaque dependencies**: DLQ store writes, AMQP/SQS/ASB receive failures, AMQP reconnect, HTTP forwarder retries, route locator failure cooldown
2. **Protocol-mandated cadence**: SQS visibility extension, ASB lock renewal, lease renewal, MQTT keepalive
3. **Operator-controlled rate limits / cache TTLs**: route locator cache TTL, outbox drainer adaptive poll, file watcher fallback poll
