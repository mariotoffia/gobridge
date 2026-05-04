# FIX — DirectHold retry pacing falls back to `RoutePolicy.Backoff`

> Companion files: `FIX-shared-outbox-completion.md`,
> `FIX-broker-restart-test-hardening.md`,
> `FIX-docker-exec-timeout.md`.

## Why this exists

When a `DeliveryDirectHold` route's outbound `Sender.Send` returns a
recoverable error that does **not** carry a `RetryAfter`, the runtime
currently asks the source transport to retry with a delay of `0`. On
SQS this means the next `ChangeMessageVisibility` is invoked with
visibility `0`, which collapses retry pacing into a tight loop until
the broker recovers — burning CPU, log volume, and metric cardinality,
and racing the broker's own reconnect window during UC42/UC43 broker
kill-restart scenarios.

`RoutePolicy.Backoff` (`BackoffPolicy{InitialInterval, MaxInterval,
Multiplier}`) is already configured exactly for this case, but the
direct-hold retry path ignores it.

## Current state

`runtime/route_runner_dispatch.go`:

```go
// First sender attempt path (~line 150)
retryAfter := domain.GetRetryAfter(sendErr)
return r.retryOrFallback(ctx, del, env, retryAfter, sendErr)

// Fallback-binding sender attempt path (~line 264)
retryAfter := domain.GetRetryAfter(err)
return r.retryOrFallback(ctx, del, env, retryAfter, err)
```

`domain.GetRetryAfter` returns zero for any error that is not a
throttled `BridgeError` carrying an explicit `RetryAfter` (verified
in `domain/errors_test.go` `TestGetRetryAfter`). Most transient
broker disconnect / I/O errors do not set `RetryAfter`, so the
runtime asks for `0`.

`retryOrFallback(ctx, del, env, after, reason)` forwards `after` into
`del.Retry(ctx, after)` (see `r.retryDelivery` immediately below the
function). The source transport faithfully retries with no delay.

## Decision

When `domain.GetRetryAfter(sendErr)` returns `0` for a recoverable
error, fall back to a value derived from `r.policy.Backoff` instead of
passing zero through.

Use the attempt-aware exponential backoff already implied by
`BackoffPolicy.Multiplier`:

```
delay = clamp(InitialInterval * Multiplier^(attempt-1), 0, MaxInterval)
```

Choose `attempt` from the existing `attempt := receiveCount(env) + 1`
that is computed elsewhere in this file. If the policy is
zero-valued, fall back to `NewDefaultBackoffPolicy()` (the runtime
already normalises `RoutePolicy` defaults at AddRoute time, so this
is mostly belt-and-suspenders).

Do not change behaviour when `RetryAfter` **is** set — the broker's
explicit hint always wins.

## Tasks

### 1. Compute pacing in one helper - DONE

Add a small helper (unexported) in `runtime/route_runner_dispatch.go`
or a new `runtime/route_runner_retry.go`:

```go
// retryDelay returns the next retry delay for a recoverable send
// error: the broker-provided RetryAfter when present, otherwise an
// exponentially-backed-off interval derived from policy.Backoff,
// capped by Backoff.MaxInterval.
func retryDelay(policy domain.RoutePolicy, attempt int, sendErr error) time.Duration {
    if d := domain.GetRetryAfter(sendErr); d > 0 {
        return d
    }
    bp := policy.Backoff
    if bp.InitialInterval <= 0 || bp.Multiplier <= 0 {
        bp = domain.NewDefaultBackoffPolicy()
    }
    if attempt < 1 {
        attempt = 1
    }
    d := float64(bp.InitialInterval)
    for i := 1; i < attempt; i++ {
        d *= bp.Multiplier
        if bp.MaxInterval > 0 && time.Duration(d) >= bp.MaxInterval {
            return bp.MaxInterval
        }
    }
    return time.Duration(d)
}
```

Replace the two zero-prone call sites:

```go
return r.retryOrFallback(ctx, del, env,
    retryDelay(r.policy, receiveCount(env)+1, sendErr), sendErr)
```

Leave the other `retryOrFallback(..., 0, ...)` call sites that pass
zero **on purpose** alone — they are DLQ/poison/override-binding
fallbacks, not transient-send retries. Read the context per call:

- DLQ-write-failed paths (lines 134, 155, 244, 255, 268, 278, 283):
  not transient send errors; keep `0` (fail-fast retry).
- Override-binding-not-found (line 45): config error; keep `0`.
- The two transient-send paths (lines 150-151, 264-265): switch to
  `retryDelay(...)`.
- The shared-outbox capacity paths (lines 400, 412, 417, 429): these
  already pass non-zero literals; leave alone or migrate to the
  helper as a follow-up.

### 2. Unit coverage - DONE

Create `runtime/route_runner_retry_test.go` (new file) with at least:

1. `TestRetryDelay_HonoursRetryAfter` — `domain.ErrThrottled.WithRetryAfter(5*time.Second)` returns `5s` regardless of attempt.
2. `TestRetryDelay_TransientErrorUsesBackoff` — plain `errors.New("conn refused")` with default `BackoffPolicy{Initial=1s, Max=30s, Mul=2}` returns `1s, 2s, 4s, 8s, 16s, 30s, 30s` for attempts 1..7.
3. `TestRetryDelay_ZeroPolicyUsesDefaults` — empty `RoutePolicy{}` still yields a non-zero delay.
4. `TestRouteRunner_DirectHoldTransientSendUsesBackoff` — table-driven runner-level test using a fake `Sender` that returns a transient error and a fake `Delivery` whose `Retry(ctx, after)` records `after`. Assert `after > 0` and `after >= policy.Backoff.InitialInterval`. Use a fake clock and `policy.MaxReplayAttempts` capped at 1 to keep the test fast.

Run:
```bash
go test -race -run 'TestRetryDelay|TestRouteRunner_DirectHoldTransientSendUsesBackoff' ./runtime/...
```

### 3. Verify with broker-restart longrunning targets

After unit tests are green, rerun the resilience suite to confirm
the behaviour change does not regress UC42/UC43 (those tests assert
collector counts rather than latency, so a non-zero retry should
still complete within their 240 s test timeout):

```bash
go test -race -timeout 1200s -v -tags=longrunning \
  -run 'TestGap_AMQP091_To_MQTT_CrossTransport|TestUC42_BrokerKillRestart_SharedOutbox|TestUC43_BrokerKillRestart_DirectHold' \
  ./tests/longrunning/...
```

Then `make test-long-running`.

## Acceptance

- `runtime/route_runner_retry_test.go` proves a non-zero retry delay
  for transient send errors lacking `RetryAfter`.
- The two transient-send call sites in
  `runtime/route_runner_dispatch.go` no longer pass literal `0`.
- `make test` and `make test-long-running` are green.

## Non-goals

- No change to `BackoffPolicy` struct shape or defaults.
- No change to the SharedOutbox drainer retry pacing — it already
  has its own scheduling via `OutboxRecord.NextEarliestSendAt`.
- Do **not** add jitter in this pass; it can be a follow-up if the
  broker-restart workloads show thundering-herd symptoms.

## Related

- `domain/route.go` — `BackoffPolicy`, `NewDefaultBackoffPolicy`.
- `domain/errors.go` / `domain/audit_errors_test.go` —
  `GetRetryAfter` semantics.
- `runtime/route_runner_dispatch.go` — `retryOrFallback`,
  `retryDelivery`, `sendDirectHold`.
