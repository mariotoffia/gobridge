# Routes, Runtime & Validation Reference

> Part of the [Configuration Model Reference](configuration-reference.md). This
> half covers routes, the `http` API block, the ID reference graph, delivery
> hooks, programmatic builder/lifecycle notes, and the validation-rules summary.

## `routes` -- Message Routes

Routes define the message flow from a receiver through processors to bindings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique route identifier |
| `receiver_id` | string | **yes** | -- | Reference to a receiver |
| `delivery_mode` | string | no | `direct_hold` | `direct_hold` or `shared_outbox` |
| `dispatch_mode` | string | no | `single` | `single` (first binding) or `fan_out` (all bindings) |
| `policy` | object | no | -- | Delivery and retry policy |
| `bindings` | string[] | **yes** | -- | References to binding IDs (at least one) |
| `processors` | string[] | no | -- | Ordered list of processor names |
| `resolver` | object | no | -- | Content-based binding resolver (see [Resolver](#routesresolver----content-based-resolver)) |
| `session` | object | no | -- | Route session management (for exclusive sessions) |

**Delivery modes:**
- **`direct_hold`** -- Source held open until egress completes. No inter-instance fencing; destinations must handle duplicates idempotently in clustered mode. When a `resolver` is configured, multiple bindings are allowed -- the resolver selects one per message.
- **`shared_outbox`** -- Source acknowledged after persisting to outbox. Outbox drainer delivers asynchronously. Requires `stores.outbox`.

**Dispatch modes:**
- **`single`** -- Send to first matching binding (or resolver-selected binding).
- **`fan_out`** -- Send to all bindings.

### `routes[].policy` -- Delivery Policy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `max_in_flight` | int | no | 100 | Max concurrent messages in this route |
| `ack_after` | string | no | `target_accept` | `target_accept` or `outbox_persist` |
| `max_replay_attempts` | int | no | 5 | Max retry attempts for failed messages |
| `max_outbox_depth` | int | no | 10000 | Max pending outbox records before backpressure |
| `on_expired` | string | no | `dlq` | `drop` or `dlq` |
| `on_permanent_failure` | string | no | `dlq` | `drop` or `dlq` |
| `send_timeout` | duration | no | `30s` | Timeout for individual send operations |
| `depth_cache_ttl` | duration | no | `1s` | How long outbox depth counts are cached |
| `allow_unfenced` | bool | no | false | Allow direct_hold with shared consumer sources (risk: no fencing) |
| `allow_retry_drop` | bool | no | false | Suppress error when source cannot retry and no DLQ is configured |

### `routes[].policy.backoff` -- Retry Backoff

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `initial_interval` | duration | no | `1s` | First retry delay |
| `max_interval` | duration | no | `30s` | Maximum retry delay |
| `multiplier` | float | no | 2.0 | Exponential backoff multiplier |

### `routes[].session` -- Route Session Management

For routes targeting exclusive sessions. Manages lease acquisition and outbox draining.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `session_id` | string | **yes** | -- | Reference to an exclusive session |
| `sender_id` | string | **yes** | -- | Reference to a sender on that session |
| `lease_ttl` | duration | no | `360s` | Lease validity duration |
| `renew_interval` | duration | no | derived | Lease renewal interval (default: lease_ttl / max_renew_fails) |
| `lease_renew_jitter` | duration | no | derived | Bounded random jitter added to each renewal timer to avoid a cluster-wide renewal thundering herd. Empty means the session manager derives it from `renew_interval`. |
| `max_renew_fails` | int | no | 3 | Consecutive renewal failures before step-down |
| `step_down_grace` | duration | no | `15s` | Grace period before releasing lease |
| `drain_interval` | duration | no | -- | Fixed outbox drain poll interval (mutually exclusive with drain_strategy) |
| `drain_batch_size` | int | no | 100 | Records per drain poll |
| `drain_max_batch_size` | int | no | 500 | Upper limit for adaptive batch scaling |
| `drain_max_concurrency` | int | no | 10 | Max concurrent send goroutines per drain cycle |
| `drain_strategy` | object | no | -- | Advanced drain polling strategy |
| `connect_after_lease` | bool | no | false | Delay transport connection until lease acquired |

When `renew_interval` is set explicitly, cross-field validation requires
`(renew_interval + lease_renew_jitter/2) × max_renew_fails < lease_ttl` so a
renewal storm can never outlast the lease (a split-brain guard). When
`renew_interval` is left empty the interval and jitter are both derived and this
check is skipped.

**High-availability profile.** The defaults above (`lease_ttl` 360s) favor low renewal traffic, so worst-case failover approaches 6 minutes. For HA deployments that need failover in the 30--60s band, use the ready-made preset `session.HAConfig` (Go API) or its equivalent recipe -- `lease_ttl: 45s`, `max_renew_fails: 3`, `step_down_grace: 5s` (derived `renew_interval` 15s), paired with the outbox store's `stale_claim_duration: 20s`. The preset encodes the required relationship between these knobs (`step_down_grace < lease_ttl` and `renew_interval × max_renew_fails ≤ lease_ttl`); hand-tuning that gets it wrong won't break single-owner safety -- the lease store and outbox version fencing guarantee that -- but it does cause spurious failovers, slower recovery, or a wider duplicate-send window. See [Scenario 8: High-Availability Profile](scenarios/08-clustered-exclusive-sessions.md#high-availability-profile) for the failover math, the invariants, and the aggressive/conservative variants.

### `routes[].session.drain_strategy` -- Drain Polling Strategy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | string | **yes** | -- | `fixed_poll` or `adaptive_backoff` |
| `interval` | duration | no | -- | Fixed poll interval (for `fixed_poll`) |
| `min_interval` | duration | no | -- | Minimum interval (for `adaptive_backoff`) |
| `max_interval` | duration | no | -- | Maximum interval (for `adaptive_backoff`) |
| `multiplier` | float | no | -- | Backoff multiplier (must be > 1.0) |

```yaml
routes:
  - id: durable-forward
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    dispatch_mode: single
    bindings: [to-sqs]
    processors: [my-filter, my-transform]
    policy:
      max_in_flight: 50
      ack_after: outbox_persist
      max_replay_attempts: 5
      on_permanent_failure: dlq
      backoff:
        initial_interval: 2s
        max_interval: 120s
        multiplier: 2.5
    session:
      session_id: mqtt-exclusive
      sender_id: sqs-out
      lease_ttl: 300s
      step_down_grace: 20s
      drain_strategy:
        type: adaptive_backoff
        min_interval: 500ms
        max_interval: 10s
        multiplier: 1.5
```

### `routes[].resolver` -- Content-Based Resolver

Configures config-driven binding selection based on envelope content (headers, subject, JSON payload). When set, the resolver determines which binding receives each message. This replaces programmatic `MatchFunc` usage for most content-based routing scenarios.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | string | **yes** | -- | Resolver type: `rules`, `header_map`, `all`, `static` |
| `default_binding` | string | no | -- | Fallback binding ID when no rule matches |
| `header_key` | string | cond. | -- | Header name to match (required for `header_map`) |
| `header_map` | map[string]string | cond. | -- | Header value to binding ID mapping (required for `header_map`) |
| `rules` | array | cond. | -- | Ordered rule list (required for `rules` type unless `default_binding` is set) |

**Resolver types:**
- **`rules`** -- Ordered first-match-wins rule evaluation. Each rule pairs conditions with a binding ID.
- **`header_map`** -- Maps a single header value directly to a binding ID. Simpler than `rules` for key-value routing.
- **`all`** -- Selects all bindings (fan-out).
- **`static`** -- Always selects the first binding.

#### `routes[].resolver.rules[]` -- Rule Definitions

Each rule is evaluated in order. The first rule whose conditions all match (AND logic) selects the binding.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `binding_id` | string | **yes** | -- | Target binding ID (must be in the route's `bindings` list) |
| `match` | array | no | -- | Conditions that must all be true. Empty `match` means the rule always matches (catch-all). |

#### `routes[].resolver.rules[].match[]` -- Condition Definitions

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `field` | string | **yes** | -- | Field to evaluate (see field patterns below) |
| `operator` | string | **yes** | -- | Comparison operator |
| `value` | any | cond. | -- | Value to compare against (not required for `exists`) |

**Field patterns:**

| Pattern | Description | Example |
|---------|-------------|---------|
| `subject` | Envelope subject | `subject` |
| `header.<key>` | Envelope header by key | `header.factory_id` |
| `$.<path>` | JSON payload path (dot-separated) | `$.metadata.region` |
| bare name | Header fallback (same as `header.<name>`) | `factory_id` |

**Operators:**

| Operator | Description | Value Type |
|----------|-------------|------------|
| `eq` | Equal (deep comparison) | any |
| `ne` | Not equal | any |
| `prefix` | String starts with | string |
| `contains` | String contains | string |
| `regex` | Regular expression match (max 4096 chars) | string |
| `gt` | Greater than (numeric) | number |
| `lt` | Less than (numeric) | number |
| `gte` | Greater than or equal | number |
| `lte` | Less than or equal | number |
| `exists` | Field exists (or not, when value is `false`) | bool |
| `in` | Value is in list | array |

**Example -- rules-based multi-tenant routing:**

```yaml
routes:
  - id: tenant-router
    receiver_id: http-in
    delivery_mode: direct_hold
    bindings: [bind-acme, bind-globex, bind-default]
    resolver:
      type: rules
      default_binding: bind-default
      rules:
        - binding_id: bind-acme
          match:
            - field: header.x-tenant
              operator: eq
              value: acme
        - binding_id: bind-globex
          match:
            - field: header.x-tenant
              operator: eq
              value: globex
```

**Example -- header_map shorthand:**

```yaml
routes:
  - id: tenant-router
    receiver_id: http-in
    bindings: [bind-acme, bind-globex]
    resolver:
      type: header_map
      header_key: x-tenant
      header_map:
        acme: bind-acme
        globex: bind-globex
```

**Example -- JSON payload routing:**

```yaml
routes:
  - id: priority-router
    receiver_id: sqs-in
    bindings: [bind-high, bind-normal]
    resolver:
      type: rules
      default_binding: bind-normal
      rules:
        - binding_id: bind-high
          match:
            - field: $.priority
              operator: eq
              value: high
```

When using a `rules` resolver with `direct_hold` delivery mode, each binding may reference a different sender. The runtime uses a **SenderRegistry** to dispatch messages to the correct sender based on the resolved binding. This enables true multi-sender routing from a single route definition.

## `http` -- HTTP API Configuration

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `admin_addr` | string | no | `:8080` | Admin server listen address |
| `monitor_addr` | string | no | `:8081` | Monitor server listen address |
| `admin_api_key` | string | **yes** | -- | API key (minimum 16 characters) |
| `monitor_api_key` | string | no | admin key | Separate monitor API key (min 16 chars when set) |
| `cors_origins` | string | no | disabled | CORS allowed origins (wildcard `*` rejected) |
| `tls_cert_file` | string | no | -- | Path to the PEM server certificate (with any intermediate chain). Enables in-process TLS on both listeners when paired with `tls_key_file`. |
| `tls_key_file` | string | no | -- | Path to the PEM private key for `tls_cert_file`. |

TLS is opt-in and both-or-none: when `tls_cert_file` and `tls_key_file` are both
set, the admin and monitor servers serve HTTPS with that pair; when either is
empty the servers stay plaintext (the historical default, assuming an external
LB/ingress/mesh terminates TLS). Supplying only one of the pair is a startup
error.

API keys are compared using SHA-256 constant-time hashing. Monitor endpoints
accept the monitor key or fall back to the admin key (admin is a superset).
Failed auth returns HTTP 401 with `WWW-Authenticate: Bearer` per RFC 9110.

See [Credentials & HTTP API](credentials-and-http-api.md) for endpoint documentation and
[OpenAPI specs](../spec/httpapi/http-api.yaml) for machine-readable definitions.

## ID Reference Graph

All IDs are validated for existence at config load time:

```mermaid
graph LR
    Route -->|receiver_id| Receiver
    Route -->|bindings[]| Binding
    Route -->|session.session_id| Session
    Route -->|session.sender_id| Sender
    Binding -->|sender_id| Sender
    Binding -.->|session_id| Session
    Receiver -.->|session_id| Session
    Sender -.->|session_id| Session

    style Route fill:#f96,stroke:#333
    style Session fill:#6bf,stroke:#333
```

Solid arrows are required references. Dashed arrows are optional. The validator rejects configs with broken references.

## Delivery Hooks (Programmatic API)

Delivery hooks are registered programmatically via the builder or runtime options -- they are not configured in YAML. A hook observes message lifecycle events without modifying the message or control flow.

### Registration

```go
hook := &myAuditHook{}

rt, err := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransport("mqtt", paho.NewFactory(logger)).
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
| `OnSettled` | `egress` | Message delivered successfully | `Err=nil`, `Terminal=true` |
| `OnSettled` | `egress` | Permanent failure routed to DLQ | `Err` set, `Terminal=true` |
| `OnSettled` | `egress` | Max retries exceeded (poison) | `Err` set, `Terminal=true` |
| `OnSettled` | `egress` | Message dropped (no DLQ, retry unsupported) | `Err` set, `Terminal=true` |
| `OnSettled` | `egress` | Message expired before send | `Err=ErrMessageExpired`, `Terminal=true` |

`OnAttempt` fires on **every** attempt including retries. `OnSettled` fires **exactly once** when the message reaches a terminal state.

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
- **Supervisor health.** `Supervisor.Degraded() (bool, string)` reports whether
  the last reconfiguration failed (with a reason) while the previous runtime
  keeps serving; `Supervisor.Terminal() bool` reports an unrecoverable state.
- **Outbox poison quarantine.** `runtime.WithOutboxPoisonMinAge` sets the minimum
  wall-clock age a record must reach *before* replay-count exhaustion may poison
  it to the DLQ, so a transient egress outage cannot burn the replay budget and
  poison healthy records in seconds. Zero (default) lets each drainer fall back
  to `max(5×send_timeout, 2m)`. This is a Go runtime option, not a YAML key.

## Validation Rules Summary

Errors from `config.Validate()`:

| Rule | Error Message |
|------|---------------|
| Missing bridge ID | `bridge.id is required` |
| Invalid deployment mode | `must be one of: standalone, clustered` |
| Duplicate IDs within section | `duplicate id "x"` |
| Broken reference | `session_id "x" not found in sessions` |
| shared_outbox without outbox store | `shared_outbox requires stores.outbox` |
| Exclusive session without lease store | `exclusive session requires stores.lease` |
| Clustered MQTT without `$share/` or exclusive | `requires $share/ topic prefix or exclusive session` |
| Invalid duration | `invalid duration "x"` |
| Mutually exclusive drain config | `drain_strategy and drain_interval are mutually exclusive` |
| Invalid resolver type | `resolver.type "x" is invalid; must be one of: rules, header_map, all, static` |
| Resolver default_binding not in route | `resolver.default_binding "x" not found in route bindings` |
| header_map missing header_key | `resolver.header_key is required for header_map type` |
| header_map empty | `resolver.header_map must have at least one entry` |
| header_map references unknown binding | `resolver.header_map["x"] references unknown binding "y"` |
| rules with no rules and no default | `resolver type "rules" requires at least one rule or a default_binding` |
| Rule references unknown binding | `binding_id "x" not found in route bindings` |
| Condition missing field | `field is required` |
| Condition invalid operator | `operator "x" is invalid` |
| Invalid regex pattern | `invalid regex pattern: ...` |
| Regex pattern too long | `regex pattern exceeds maximum length of 4096 characters` |

**Warnings** (non-fatal):
- `direct_hold` advisory: no inter-instance fencing, destination must handle duplicates
- `stale_claim_duration` too large relative to `step_down_grace`
