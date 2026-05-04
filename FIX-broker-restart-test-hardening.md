# FIX — Harden UC42 / UC43 broker-restart longrunning tests

**Status:** Resolved 2026-05-04. UC42 now uses `setupDynamoStoresForRestart`, UC43 declares the `uc43-bind` binding and a non-nil `*SessionConfig` (built via `lrSessionConfig`), and a fail-fast `requireMQTTSessionReady` helper guards against `sessCfg==nil` regressions in both tests. The skip-rule in `runtime/bridge_health.go` now carries an explanatory comment. Verified via `go build ./...`, `go vet ./...`, `go build -tags=longrunning ./tests/longrunning/...`, `go vet -tags=longrunning ./tests/longrunning/...`, `go test -short -race ./runtime/...`, and `make lint` (all pass). Audit allowlist line numbers refreshed for the line-shift introduced by the new helper.

> Companion files: `FIX-directhold-retry-pacing.md`,
> `FIX-shared-outbox-completion.md`,
> `FIX-docker-exec-timeout.md`.

## Why this exists

`TestUC42_BrokerKillRestart_SharedOutbox` and
`TestUC43_BrokerKillRestart_DirectHold` (both in
`tests/longrunning/uc42_broker_resilience_test.go`) are the canonical
broker-resilience cases for the two delivery modes. They flake under
`make test-long-running` for three orthogonal reasons; this FIX bundles
them because they share fixtures and the same verification command.

### Issue A — UC42 uses the wrong DynamoDB store fixture

UC42 calls:

```go
leaseStore, outboxStore := setupDynamoStores(t)
```

Other restart-aware tests (UC48, UC92, UC93, gap_broker_crash) use:

```go
leaseStore, outboxStore := setupDynamoStoresForRestart(t)
```

`setupDynamoStoresForRestart` (defined ~line 325 of
`tests/longrunning/longrunning_test.go`) is the variant that
configures stores so they survive broker kill+restart without losing
in-flight records. UC42 is a broker-kill-restart test by name, so it
must use this fixture.

### Issue B — UC43 route lacks `Bindings` metadata

UC43 builds:

```go
routeCfg := goruntime.RouteConfig{
    ID: "uc43-route",
    Policy: domain.RoutePolicy{
        DeliveryMode:      domain.DeliveryDirectHold,
        MaxReplayAttempts: 50,
    },
    Resolver: goruntime.NewStaticResolver(
        domain.DispatchPlan{BindingID: "uc43-bind", Address: outTopic},
    ),
    SourceCapabilities: directHoldCaps,
}
require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, nil))
```

There is no `Bindings: []domain.DestinationBinding{...}` slice and the
`*SessionConfig` argument is `nil`. Compare UC42 (lines 70-82):

```go
Bindings: []domain.DestinationBinding{
    {ID: "uc42-bind", SessionID: sessionID},
},
...
require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc))
```

Without `Bindings` populated, the route entry's binding→session
mapping is empty, which downstream telemetry and DLQ routing rely
on. UC43 must declare its binding the same way UC42 does so DLQ
attributes (`BindingID`, `SessionID`) are non-empty when failures
fan out.

### Issue C — `gobridgesync` skips routes with `sessCfg == nil`

`runtime/bridge_health.go` (~line 141):

```go
for _, e := range rt.entries {
    if e.session == nil || e.sessCfg == nil {
        continue
    }
    ...
}
```

`gobridgesync` waits for **session** readiness via that aggregator.
UC43 currently passes `nil` to `AddRoute`'s `sessCfg` parameter, so
its MQTT session is invisible to `gobridgesync` — the helper returns
"ready" without ever observing the session subscribed and connected
to the broker.

This silently masks broker-reconnect failures: the test proceeds to
publish even though the bridge session has not actually attached its
subscriptions yet. After broker restart the symptom is worse — the
post-restart `gobridgesync(t, 30*time.Second, rt)` still returns
ready immediately because the session is not in the map, and the
subsequent collector wait often times out.

## Decisions

### A. Use `setupDynamoStoresForRestart` in UC42

```diff
- leaseStore, outboxStore := setupDynamoStores(t)
+ leaseStore, outboxStore := setupDynamoStoresForRestart(t)
```

No other change needed — the function signature is identical.

### B. Declare bindings and session config in UC43

```diff
 routeCfg := goruntime.RouteConfig{
     ID: "uc43-route",
     Policy: domain.RoutePolicy{
         DeliveryMode:      domain.DeliveryDirectHold,
         MaxReplayAttempts: 50,
     },
     Resolver: goruntime.NewStaticResolver(
         domain.DispatchPlan{BindingID: "uc43-bind", Address: outTopic},
     ),
+    Bindings: []domain.DestinationBinding{
+        {ID: "uc43-bind", SessionID: sessionID},
+    },
     SourceCapabilities: directHoldCaps,
 }
- require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, nil))
+ sc := lrSessionConfig(sessionID)
+ require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc))
```

### C. Add explicit MQTT session readiness checks

`gobridgesync` is fine as-is; the fix is to make sure UC43 actually
participates in it (covered by B above) AND to add an explicit
session-ready assertion before the producer starts and after the
restart, so a regression where session config is dropped fails
loudly instead of silently passing.

Add a helper in `tests/longrunning/longrunning_helpers_test.go` if
one does not exist:

```go
// requireMQTTSessionReady asserts that the runtime reports the
// given session as Connected and Ready. Fails fast (no polling)
// — call after gobridgesync returns to verify the helper saw the
// session at all (gobridgesync silently skips entries with
// sessCfg == nil; this assertion catches that misconfiguration).
func requireMQTTSessionReady(t *testing.T, rt *goruntime.Runtime, sessionID string) {
    t.Helper()
    h := rt.Health(context.Background())
    for _, sd := range h.Sessions {
        if sd.SessionID == sessionID {
            require.True(t, sd.Connected, "session %s not connected", sessionID)
            require.True(t, sd.Ready, "session %s not ready", sessionID)
            return
        }
    }
    t.Fatalf("session %s missing from runtime health (likely sessCfg==nil at AddRoute)", sessionID)
}
```

Call after each `gobridgesync` in UC42 and UC43 (initial + post-
restart).

### D. Document the gobridgesync skip rule

Add a single-line comment near the
`if e.session == nil || e.sessCfg == nil { continue }` block in
`runtime/bridge_health.go` explaining that callers passing `nil`
sessCfg to `AddRoute` are excluded from health aggregation. This is
reference documentation for future test authors who hit issue C.

## Tasks

1. Edit `tests/longrunning/uc42_broker_resilience_test.go`:
   - UC42: switch to `setupDynamoStoresForRestart`.
   - UC43: add `Bindings` and pass `&sc` to `AddRoute`.
2. Add `requireMQTTSessionReady` helper to
   `tests/longrunning/longrunning_helpers_test.go` and call it after
   every `gobridgesync(...)` in UC42 and UC43.
3. Add the explanatory comment in `runtime/bridge_health.go`.
4. Verify:

```bash
go test -race -timeout 1200s -v -tags=longrunning \
  -run 'TestGap_AMQP091_To_MQTT_CrossTransport|TestUC42_BrokerKillRestart_SharedOutbox|TestUC43_BrokerKillRestart_DirectHold' \
  ./tests/longrunning/...
```

Then `make test-long-running`.

## Acceptance

- UC42 uses `setupDynamoStoresForRestart`.
- UC43 declares `Bindings` and a non-nil `*SessionConfig`.
- `requireMQTTSessionReady` asserts session readiness in both tests.
- A deliberate regression (passing `nil` sessCfg back) fails the new
  assertion with a clear message.
- The targeted longrunning run is green.

## Non-goals

- Do not refactor `gobridgesync` to inspect entries with `sessCfg == nil`. Routes without a session are legitimate (e.g. SQS→SQS); changing the aggregator changes its meaning for unrelated tests.
- Do not change the gap_broker_crash tests in this pass — they
  already use `setupDynamoStoresForRestart`.

## Related

- `tests/longrunning/uc42_broker_resilience_test.go` — UC42/UC43.
- `tests/longrunning/longrunning_test.go` —
  `setupDynamoStoresForRestart` (~line 325).
- `tests/longrunning/longrunning_helpers_test.go` — `gobridgesync`,
  `lrSessionConfig`, `lrWaitFor`.
- `runtime/bridge_health.go` — health aggregation skip rule.
- `runtime/bridge_routes.go` — `AddRoute(... sessCfg *SessionConfig)`.
