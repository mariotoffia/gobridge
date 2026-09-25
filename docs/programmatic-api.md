# Programmatic API: Delivery Hooks, Builder and Lifecycle

Embedding GoBridge in Go code: delivery hooks, the programmatic builder, and
runtime lifecycle notes. Split out of
[Routes, Runtime & Validation Reference](routes-and-runtime-reference.md),
which is the declarative configuration reference.

## Delivery Hooks (Programmatic API)

Delivery hooks are registered programmatically via the builder or runtime options -- they are not configured in YAML. A hook observes message lifecycle events; it cannot modify the message or change the settlement outcome (the callbacks have no return value the runtime acts on). It is not free: hooks run **synchronously on the delivery goroutine**, so a slow or blocking hook directly adds delivery latency and can stall the route. A panic in `OnAttempt`/`OnSettled` is contained by an internal recover (counted on the delivery-panic metric with `reason=hook` and logged) so it never alters settlement or produces a duplicate -- but keep hooks fast and non-blocking rather than relying on that.

### Registration

```go
hook := &myAuditHook{}

rt, err := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransportFactory("mqtt", paho.NewFactory(logger)).
    RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
    RegisterDeliveryHook(hook).
    Build(ctx)
```

Or at the runtime level:

```go
rt := runtime.New(
    runtime.WithDeliveryHook(hook),
    // ... other options
)
```

### Interface

```go
type DeliveryHook interface {
    OnAttempt(ctx context.Context, evt DeliveryAttempt)
    OnSettled(ctx context.Context, evt DeliveryOutcome)
}
```

### When hooks fire

| Event | Direction | When | Fields |
|-------|-----------|------|--------|
| `OnAttempt` | `ingress` | Every time a message is received from a source transport | `RouteID`, `Envelope`, `Attempt=1` |
| `OnAttempt` | `egress` | Every send attempt (DirectHold) or drain attempt (SharedOutbox) | `RouteID`, `BindingID`, `Envelope`, `Attempt`, `MaxAttempts`, `Err` |
| `OnSettled` | `egress` | Delivered successfully (DirectHold send or SharedOutbox drain) | `Err=nil`, `Terminal=true` |
| `OnSettled` | `egress` | DirectHold send or SharedOutbox drain failed permanently -- DLQ/drop | `Err` set, `Terminal=true` |
| `OnSettled` | `egress` | DirectHold send or SharedOutbox drain hit the replay cap (poison) -- DLQ/drop | `Err` set, `Terminal=true` |
| `OnSettled` | `ingress` | Permanent processor/resolve failure -- DLQ/drop | `Err` set, `Terminal=true` |
| `OnSettled` | `ingress` | Replay cap reached on the processor/resolve/outbox-build path (poison) -- DLQ/drop | `Err` set, `Terminal=true` |
| `OnSettled` | `ingress` | Message filtered by a processor -- drop/DLQ | `Err=ErrMessageFiltered`, `Terminal=true` |
| `OnSettled` | `ingress` | Message dropped (retry unsupported, no DLQ) | `Err` set, `Terminal=true` |
| `OnSettled` | `ingress` | Message expired before send | `Err=ErrMessageExpired`, `Terminal=true` |

Terminal `Direction` reflects where the message settled. Outcomes on the send path -- a successful send, or a DirectHold send or SharedOutbox drain that failed permanently or hit the replay cap -- are stamped `egress`. Outcomes that settle at the source boundary before or without a successful egress hop -- expired, filtered, a permanent processor/resolve error, a retry-unsupported drop, or a replay-cap poison on the processor/resolve/outbox-build path -- converge through the runtime's `settleTerminal` and are stamped `ingress`. Dashboards and audit rules that key on `Direction` must expect `ingress` for these, not `egress`.

`OnAttempt` fires on **every** attempt including retries. `OnSettled` fires after the message reaches a terminal state — for the SharedOutbox path, after the terminal store transition Completes. A failed Complete re-claims the record and defers the hook to the successful retry, so `OnSettled` never double-fires; conversely a crash in the window between a durable Complete and the hook can skip it for that one record (the settlement itself stays durable). Treat it as **at-most-once per completed record**, not exactly once.

### Event structs

- `DeliveryAttempt.Attempt` -- 1-based attempt number. For DirectHold this is `receiveCount + 1`; for SharedOutbox this is `replayCount + 1`.
- `DeliveryAttempt.MaxAttempts` -- from the route policy `max_replay_attempts`. Zero means unknown.
- `DeliveryAttempt.Err` -- nil on successful attempt, non-nil on failure.
- `DeliveryOutcome.Terminal` -- always `true` (distinguishes settled events from attempt events in shared logging code).

### Thread safety

Hook methods may be called concurrently from multiple delivery goroutines. Implementations must be safe for concurrent use. Hooks are called synchronously on the delivery goroutine -- a slow hook directly increases delivery latency.

### Hooks vs Processors

Hooks and processors serve different purposes:

| Concern | Processor | Hook |
|---------|-----------|------|
| Can mutate the envelope | Yes | No |
| Can short-circuit the pipeline | Yes | No |
| Called per attempt or per message | Per message (before send) | Per attempt and on final outcome |
| Registration | Config YAML (`processors:`) | Programmatic (`RegisterDeliveryHook`) |
| Use case | Filtering, transformation, enrichment | Audit logging, observability, external notification |

### Example: audit logging hook

```go
type auditHook struct {
    logger *slog.Logger
}

func (h *auditHook) OnAttempt(ctx context.Context, evt ports.DeliveryAttempt) {
    if evt.Direction == ports.DirectionEgress && evt.Err != nil {
        h.logger.Warn("egress attempt failed",
            "route", evt.RouteID,
            "binding", evt.BindingID,
            "envelope_id", evt.Envelope.ID,
            "attempt", evt.Attempt,
            "max_attempts", evt.MaxAttempts,
            "error", evt.Err,
        )
    }
}

func (h *auditHook) OnSettled(ctx context.Context, evt ports.DeliveryOutcome) {
    level := slog.LevelInfo
    if evt.Err != nil {
        level = slog.LevelError
    }
    h.logger.Log(ctx, level, "delivery settled",
        "route", evt.RouteID,
        "binding", evt.BindingID,
        "envelope_id", evt.Envelope.ID,
        "attempts", evt.Attempt,
        "error", evt.Err,
    )
}
```

## Programmatic Builder & Lifecycle Notes

These affect the Go composition root (`bridge.Builder` / `bridge.Supervisor`),
not the YAML shape, but they change *when* and *how* config errors surface:

- **Route validation runs at Build time.** `Builder.Build` runs the runtime's
  static route validation (`Runtime.ValidateRoutes`) during construction --
  while any previous runtime is still serving -- so a statically-rejectable
  config fails at `Build(ctx)` rather than later at `Start`. `Start` re-runs the
  same checks as a backstop. *(Breaking: errors that previously surfaced at
  `Start` now surface at `Build`.)*
- **Removed supervisor knobs.** `WithDefaultPerRecordDrainTimeout` and
  `WithDefaultMaxDrainTimeout` were removed (they had no effect). The scaled
  drain formula is configured through the blueprint's
  `bridge.per_record_drain_timeout` / `bridge.max_drain_timeout` instead.
- **Observability wiring.** Inject exporters via `bridge.WithMetrics`,
  `bridge.WithTracer`, and `bridge.WithAuditLogger` on the `Builder` (or the
  `WithSupervisor*` equivalents on the `Supervisor`, which forward to every
  `Builder`/`Runtime` it creates). Without them a config-driven deployment runs
  the no-op exporters and emits nothing.
- **Route readiness is pipeline liveness, not delivery success.** A route's
  `ready` flag (deep health, and the `full` readiness level) means its runner and
  receiver are up and accepting work. It does **not** mean recent deliveries
  reached their target: a route whose destination refuses 100% of sends stays
  `ready` while it retries, DLQs, or hands messages back to the source. That is
  deliberate — a probe that flipped on target availability would eject the one
  instance that is correctly retrying, and every instance at once during a
  shared-target outage. Alert on delivery instead: `RouteErrors` (`route_id`) is
  the delivery-stall signal, alongside `DLQEntries`, `MessagesDropped`, and
  `OutboxDepth` for the `shared_outbox` mode. `RouteErrors` covers recoverable
  send failures, recoverable processor-chain failures and a resolver that
  returned more than one plan; of those, the **send** failures on a
  `direct_hold` route are counted only once the in-process send retry gives up,
  so that one signal lags a stalling destination by up to `send_retry_budget`
  plus the last send's hold, which is at most one send wedge ceiling
  (`send_timeout` + `min(send_timeout, 5s)`): 95s with the defaults. `SendRetries`
  (`route_id`) is the earlier signal.
  `route_dead` (a route flapping at the supervisor backoff cap) is the separate
  *pipeline* fault state.
- **`Inject` / `InjectToBinding` block until the message settles.** Both are
  synchronous: they return when the route delivered the message or settled it
  terminally. On a `direct_hold` route a recoverable send failure is now retried
  inside the bridge first, so once the call has a free in-flight slot on the
  route it can block for up to
  `processors × processor_timeout + send_retry_budget + send wedge ceiling + 10.5s`.
  The last send starts inside `send_retry_budget`, and a sender that ignores its
  context holds it until the send wedge ceiling, `send_timeout` +
  `min(send_timeout, 5s)`. The 10.5 seconds is the dead-letter write (two
  5-second write attempts with a 500 ms pause between them), paid only when the
  route writes the message to the dead-letter store; if that store is itself
  failing, a second write can add up to 10.5 seconds more. With the defaults and no
  processors the bound is 60s + 35s + 10.5s = 105.5s. Waiting for the in-flight
  slot comes first, and only your context limits it.

  **Your context is the real cap.** When it ends, the retry loop stops and the
  call returns an error that wraps your context's error, without dead-lettering
  the message: the delivery is abandoned. A send already in progress is still
  waited for, up to the send wedge ceiling when the sender ignores its context.
  Give the call a context whose deadline you are willing to wait for, and
  remember the admin DLQ redrive inherits this: its batch of sequential injects
  has 30 seconds, and each entry's lookup and inject get at most 10 seconds of
  it.
- **A hand-wired route names its source session.** When you call
  `Runtime.AddRoute` yourself, set `RouteConfig.SourceSessionID` to the session
  the route's receiver subscribes through. The runtime uses it to name the route
  on a record dead-lettered for a removed subscription, and a manual or automatic
  redrive of that record needs the route (ADR 0019). The session you pass to
  `AddRoute` is not used for this, because it can be an egress session: naming
  the route from it would redrive the messages to that route's destination.
  Leave the field empty and such records carry no route; they stay in the DLQ
  for you to handle.
- **Route fault blast radius.** A route whose receiver fails is restarted in
  isolation, whatever its transport: backed off, counted on `RouteRestarts`,
  marked not-ready, and latched `route_dead` after repeated quick flaps. Every
  other route keeps running. A receiver with `Close(ctx)` (Service Bus, AMQP
  1.0, AMQP 0-9-1) is closed when its run ends, which settles what it held, and
  attaches again on the next run. So a queue that is deleted, or access that is
  revoked, on one owner's broker stops only that route, and the route recovers
  on its own once the broker is fixed. A receiver you write yourself must accept
  `Run` after `Close` (see `ports.Receiver`). A route makes the runtime terminal
  only when it is **wedged** — a hung sender, too many abandoned processor
  goroutines, or a `shared_outbox` route with no outbox store — or when its
  receiver panics, which is a bug and fails fast. A restart in the process
  cannot clear a wedge: Go cannot stop a leaked goroutine, and a missing store
  is a wiring fault. The backstop there is a process restart (`/live` fails
  closed).
- **Supervisor health.** `Supervisor.Degraded() (bool, string)` reports whether
  the last reconfiguration failed (with a reason) while the previous runtime
  keeps serving; `Supervisor.Terminal() bool` reports an unrecoverable state.
- **Outbox poison quarantine.** Poisoning a record to the DLQ requires BOTH
  `max_replay_attempts` exceeded AND the wall-clock `replay_budget` spent,
  measured from the record's first delivery attempt (`FirstAttemptedAt`). A
  record's replay count increments on every *claim* — including batch-deadline
  deferrals and stale-claim reclaims where no send ever failed — so replay
  exhaustion alone is never sufficient: a transient egress outage that burns the
  count in seconds cannot poison a healthy record until real time has elapsed.
  The gate is a hard AND, never an OR. A record that is somehow claimed without a
  first-attempt stamp reports its budget UNSPENT and keeps being retried, so a
  store that breaks the stamp contract can never destroy a message.
