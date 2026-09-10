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
  `OutboxDepth` for the `shared_outbox` mode. `route_dead` (a route flapping at
  the supervisor backoff cap) is the separate *pipeline* fault state.
- **Route fault blast radius.** A route whose receiver fails is restarted in
  isolation — backed off, counted on `RouteRestarts`, marked not-ready, and
  latched `route_dead` after repeated quick flaps — only when the source can be
  re-entered. That holds for SQS, MQTT and HTTP, whose broker client belongs to
  the session. A source the route runner must close on exit (Service Bus, AMQP
  1.0, AMQP 0-9-1) is single-use: the runtime has no factory to rebuild it from,
  so the route escalates to a terminal runtime and the backstop is a **process
  restart** with freshly-built transports (`/live` fails closed). Size the
  restart budget for those transports accordingly; `route_dead` never latches
  for them.
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
