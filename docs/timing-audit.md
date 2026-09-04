# Timing Audit

Every wait in production code (`time.Sleep`, `time.After`, `time.NewTicker`,
`time.NewTimer`, `context.WithTimeout`) must go through
`domain/clock.Clock` and be cancellable via `ctx`. This document describes
the invariant, the enforcement, and the readiness APIs that replaced
sleep-based test synchronisation.

## Classification key

| Class | Meaning | Action |
|---|---|---|
| **REPLACEABLE** | Sleep substitutes for a missing signal | Add the missing event/channel/hook; delete the sleep |
| **DEADLINE** | Upper bound on a bounded operation | Derive from operational config; never hardcode |
| **ESSENTIAL** | Genuine waiting (backoff, protocol cadence) | Keep, make cancellable via `select { ctx; clk.After }`, expose in config, route through `Clock` |

## Current state

**Production**: zero unauthorized timing calls. Every wait across
`adapters/`, `runtime/`, `bridge/`, `processors/`, `circuitbreaker/`,
and `httpapi/` routes through `domain/clock.Clock`. The `Clock`
interface deliberately omits `Sleep` — every wait is
`select { case <-ctx.Done(): case <-clk.After(d): }`, enforcing
cancellability at the type level.

**Tests**: `audit/test-timing-allowlist.txt` tracks the small
irreducible set of `time.Sleep` calls in tests, each annotated with
its classification comment (e.g. `// ESSENTIAL: protocol cadence`,
`// OTHER: real-time component of clocktest.Fake`).

An entry matches on the FILE and the CODE, not on the line number it
carries. The line numbers are provenance, not part of the key: an
allowed sleep that moves down its file because something above it was
edited is the same allowed sleep, and requiring a renumber there
failed the audit on files nobody had touched. Changing the sleep
itself, or moving it to another file, still needs a new entry — the
exemption is for that call, not for a path.
`scripts/audit-timing-filter.awk` implements the match.

**Enforcement**: `make audit-timings` and `make audit-test-timings`
run on every `make test` / `make check` / `make test-integration` and
fail the build on new unauthorized entries. See the header comments
in `audit/timing-allowlist.txt` and `audit/test-timing-allowlist.txt`
for how to justify a new entry.

## Irreducible ESSENTIAL set

These are the only legitimate kinds of wait that survive event-signal
elimination:

1. **Backoff against opaque dependencies** — DLQ store writes,
   AMQP/SQS/ASB receive failures, AMQP reconnect, HTTP forwarder
   retries, route locator failure cooldown.
2. **Protocol-mandated cadence** — SQS visibility extension, ASB
   lock renewal, lease renewal, MQTT keepalive.
3. **Operator-controlled rate limits / cache TTLs** — route locator
   cache TTL, outbox drainer adaptive poll, file watcher fallback
   poll.

Anything outside these three categories should be driven by an event
signal. The readiness API inventory below is the catalogue of those
signals.

## Readiness API inventory

| Component | API | Signal type | When signaled | File |
|---|---|---|---|---|
| `ports.Receiver` (optional) | `Started() <-chan struct{}` | Channel close | Receive loop live | `ports/transport.go` |
| `ports.Session` | `Health(ctx) SessionHealth` | Struct poll | Anytime | `ports/transport.go` |
| `ports.Session` | `Events() <-chan SessionEvent` | Event push | Connect/disconnect/reconcile | `ports/transport.go` |
| `ports.SessionHealth` | `ActiveTopics []string` / `HasTopic()` | Struct field | From session `Health()` | `ports/transport.go` |
| `runtime.Runtime` | `DeepHealth(ctx) DeepHealth` | Struct poll | Anytime | `runtime/bridge.go` |
| `runtime.Runtime` | `WaitRouteReady(ctx, routeID) error` | Event-driven wait | Route runner + receiver `Started()` closed (sanity fallback 1s) | `runtime/bridge.go` |
| `runtime.Runtime` | `WaitQuiescent(ctx, opts) error` | Event-driven wait | Per-route `IdleChanged()` fan-in; zero in-flight for `MinQuiet` | `runtime/bridge.go` |
| `runtime.Runtime` | `ReadinessLevel(ctx) ReadinessLevel` | Snapshot enum | Down/Live/Running/Connected/Subscribed/Full | `runtime/bridge.go` |
| `runtime.Runtime` | `AtLeast(ctx, want) bool` | Snapshot bool | Has reached requested level | `runtime/bridge.go` |
| HTTP `/api/v1/monitor/ready?level=` | level= query param | HTTP 200/503 | live, running, connected, subscribed, full | `httpapi/monitor.go` |
| HTTP `/api/v1/monitor/deephealth` | JSON body | Per-session `active_topics`, per-route `ready` + `in_flight`, current `level` | exposed in response | `httpapi/monitor.go` |
| `runtime.RouteRunner` | `Started() <-chan struct{}` | Channel close | `Run()` entered | `runtime/route/runner.go` |
| `runtime.RouteRunner` | `InFlight() int64` | Atomic counter | Per delivery enter/exit | `runtime/route/runner.go` |
| `runtime.RouteRunner` | `IdleChanged() <-chan struct{}` | Channel close + swap | InFlight transitions → 0 | `runtime/route/runner.go` |
| `runtime.RouteHealth` | `Ready bool` / `InFlight int` | Struct fields | From `DeepHealth()` | `runtime/bridge.go` |
| `runtime.OutboxDrainer` | `IdleSince() (time.Time, bool)` | Atomic timestamp | Pending → empty transition | `runtime/outbox/drainer.go` |
| `runtime.OutboxDrainer` | `WaitIdle(ctx, minQuiet) error` | Blocking wait | Continuously idle for `minQuiet` | `runtime/outbox/drainer.go` |
| `runtime.SessionManager` | `LeaseStateChanged() <-chan LeaseStateEvent` | Event push | Acquire/renew/release/stepdown/loss | `runtime/session/manager.go` |
| `paho.Receiver` | `Started() <-chan struct{}` | Channel close | Handler registered on router | `adapters/mqtt/transport/paho/receiver.go` |
| `paho.Session` | `Health()` / `Events()` | Poll + event | Connect/reconcile/disconnect | `adapters/mqtt/transport/paho/session.go` |
| `http.SSESender` | `WaitClientConnected(ctx, n) error` | Blocking wait | N clients connected | `adapters/http/transport/sender_sse.go` |
| `file.Watcher` | `Started() <-chan struct{}` | Channel close | fsnotify registered | `adapters/native/config/file/acl_watcher.go` |
| `file.Watcher` | `LastApplied() time.Time` | Atomic timestamp | Config successfully applied | `adapters/native/config/file/acl_watcher.go` |

## Integration-test content verification

The `testutil/testcontent` package provides deterministic message
content verification. Every test message is tagged with a unique TID
(test-message-ID) in both a header (`x-bridge.test-msg-id`) and a
JSON payload field (`_tid`).

Assertion helpers compare sent vs received sets by TID:

- `AssertReceivedSet` — set(sent.TID) == set(received.TID); reports missing and extra
- `AssertContentMatches` — set equality + per-TID payload/header comparison
- `AssertNoDuplicates` — each TID appears at most once
- `AssertOrdered` — received TIDs appear in the same order as sent

The `ExtractTID` function tries the header first, then falls back to
the JSON `_tid` field, so it works even when transport headers are
stripped (e.g. SQS → body-only polling).

**MQTT Envelope.ID round-trip**: `PublishFromEnvelope` stamps the
`Envelope.ID` as a `mqtt.message-id` user property, so a peer bridge's
producer identity survives the hop. `EnvelopeFromPublish` recovers the
inbound identity in precedence order:

1. a valid `mqtt.message-id` user property (the producer ID a peer stamped);
2. valid MQTT correlation data (`x-bridge.correlation-id`);
3. a per-publish RFC 4122 UUIDv4, generated once for the received publish
   (`newIngressEnvelopeID`). This is **not** a topic+payload hash: two
   legitimate equal-valued publishes with no producer ID get distinct IDs, so
   `shared_outbox` does not silently collapse them. See
   [MQTT — Envelope identity and no-ID redelivery](transports/mqtt-ingress-headers.md#envelope-identity-and-no-id-redelivery).

This enables `countUnique()` to work correctly on MQTT collectors for
all throughput, resilience, and backpressure tests.
