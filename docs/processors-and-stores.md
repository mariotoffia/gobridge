# Processors and Store Backends

This reference covers the GoBridge processor chain and the backing store
backends for lease coordination, outbox persistence, and dead-letter queuing.

## Processor Chain Overview

Processors are registered by name in Go and referenced from route definitions.
There is no top-level `processors:` config map — a processor's type and settings
are supplied when you construct it in Go (see [Programmatic Registration](#programmatic-registration));
the only processor-related YAML is the per-route `processors` list of registered names:

```yaml
routes:
  - id: my-route
    processors: [my-filter, my-transform, my-cb, my-tenant]
```

Every processor implements `ports.Processor`:

```go
type ProcessorFunc func(ctx context.Context, env *messaging.Envelope) error

type Processor interface {
    Name() string
    Process(ctx context.Context, env *messaging.Envelope, next ProcessorFunc) error
}
```

Processors execute as an **onion model** -- each wraps the next and calls
`next(ctx, env)` to continue the chain. A processor that does not call `next`
short-circuits the *remaining* chain successfully -- the envelope still
dispatches to the sender. Only returning an error stops dispatch; only
`shared.ErrMessageFiltered` marks an intentional drop (counted as
`MessagesFiltered`, settled `OnSettled(Terminal: true)`, acked, not a route
error).

```mermaid
flowchart LR
    E[Envelope] --> F[Filter]
    F --> T[Transform]
    T --> CB[Circuit Breaker]
    CB --> TN[Tenant]
    TN --> D[Dispatch to sender]
    style E fill:#f9f,stroke:#333
    style D fill:#2d6,stroke:#333
```

---

## Filter Processor

**Package:** `processors/filter` | **Config type:** `filter.Config`

Evaluates conditions against the envelope and applies an action: pass the
message through, drop it, or reroute it.

### Configuration Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `Name` | `string` | No | Unique processor instance name. Defaults to `"filter"`. |
| `Conditions` | `[]Condition` | Yes | List of conditions to evaluate (AND logic). |
| `Action` | `Action` | Yes | One of `pass`, `drop`, `route`. |
| `RouteTo` | `string` | Only when `Action` is `route` | Target route ID for rerouting. |
| `Invert` | `bool` | No | Inverts the combined condition result. |
| `MaxPayloadBytes` | `int` | No | Caps the JSON payload the filter will parse. Non-positive selects `DefaultMaxPayloadBytes`. |

### Condition Fields

| Field | Type | Description |
|-------|------|-------------|
| `Field` | `string` | Field to match against (see patterns below). |
| `Operator` | `Operator` | Comparison operator. |
| `Value` | `any` | Comparison value. |

**Field patterns:**

- `subject` -- matches `Envelope.Subject`
- `header.<key>` -- matches `Envelope.Headers["<key>"]`
- `$.<path>` -- dot-path traversal into JSON `Envelope.Payload`
- bare name -- falls back to `Envelope.Headers` lookup

**Operators:** `eq`, `ne`, `contains`, `regex`, `gt`, `lt`, `gte`, `lte`,
`exists`, `in`

### Go Example

A dropped message returns `shared.ErrMessageFiltered`, which the runtime
classifies as an intentional filter discard (counted as `MessagesFiltered`,
not a route error).

```go
proc, err := filter.New(filter.Config{
    Name: "my-filter",
    Action: filter.ActionDrop,
    Conditions: []filter.Condition{
        {Field: "header.x-env", Operator: filter.OperatorEquals, Value: "staging"},
        {Field: "$.priority", Operator: filter.OperatorLessThan, Value: 3},
    },
})

// Convenience constructors
drop, _ := filter.NewDropFilter("drop-staging", filter.Condition{...})
pass, _ := filter.NewPassFilter("only-prod", filter.Condition{...})
reroute, _ := filter.NewRouteFilter("reroute-high", "high-priority-route", filter.Condition{...})
```

---

## Transform Processor

**Package:** `processors/transform` | **Config type:** `transform.Config`

Reshapes JSON payloads using JSONPath extraction and field mappings.

### Configuration Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Name` | `string` | `"transform"` | Unique processor instance name. |
| `Mappings` | `[]FieldMapping` | -- | Field transformation rules (required). |
| `DropUnmapped` | `bool` | `false` | Drop fields not covered by mappings. |
| `FailOnError` | `bool` | `false` | Fail the message on any transform error. |
| `MaxPayloadBytes` | `int` | `DefaultMaxPayloadBytes` | Caps the JSON payload this processor will parse. |

### FieldMapping Fields

| Field | Type | Description |
|-------|------|-------------|
| `Source` | `string` | JSONPath expression, e.g. `$.user.name` |
| `Target` | `string` | Target field in dot notation, e.g. `output.username` |
| `Transform` | `TransformType` | Type conversion (see below). Optional. |
| `DefaultValue` | `any` | Fallback value if source is not found. Optional. |
| `Required` | `bool` | Fail the message if this mapping cannot be resolved. |

**Transform types:** `string`, `int`, `float`, `bool`, `base64encode`,
`base64decode`

### Go Example

```go
proc, err := transform.New(transform.Config{
    Name:         "my-transform",
    DropUnmapped: true,
    Mappings: []transform.FieldMapping{
        transform.SimpleMapping("$.user.name", "username"),
        transform.TransformedMapping("$.user.age", "userAge", transform.TransformInt),
        {Source: "$.metadata.token", Target: "auth.token",
         Transform: transform.TransformBase64Encode, Required: true},
    },
})
```

When `DropUnmapped` is `false` (default), original payload fields are preserved
and mappings are applied on top. When `true`, only mapped fields appear in output.

---

## Circuit Breaker Processor

**Package:** `circuitbreaker` (standalone state machine) + `processors/circuitbreaker` (processor wrapper) | **Config type:** `circuitbreaker.Config`

Tracks downstream failures per key and short-circuits requests when a breaker
trips open, protecting the system from hammering a failing dependency.

### State Machine

```mermaid
stateDiagram-v2
    [*] --> Closed

    Closed --> Open : consecutive failures >= FailureThreshold
    Open --> HalfOpen : ResetTimeout elapsed
    HalfOpen --> Closed : consecutive successes >= SuccessThreshold
    HalfOpen --> Open : any countable failure

    Closed --> Closed : success / non-countable error
    Open --> Open : requests rejected (ErrUnavailable)
```

### Configuration Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `FailureThreshold` | `int` | `5` | Consecutive countable failures before opening. |
| `SuccessThreshold` | `int` | `2` | Consecutive successes in half-open to close. |
| `ResetTimeout` | `time.Duration` | `30s` | Time in open state before transitioning to half-open. |
| `HalfOpenMaxProbes` | `int` | `1` | Max concurrent probe requests allowed in half-open. |
| `CountError` | `ErrorClassifier` | counts transient errors | Function that returns true if an error should count toward the failure threshold. |

By default, only **transient (recoverable) errors** trip the breaker.
Permanent errors such as invalid payload or authentication failures are not
counted, preventing bad input from opening a breaker on a healthy dependency.

### Key Extraction Options

Circuit breakers are partitioned by key. The key extractor determines granularity:

| Option | Behavior |
|--------|----------|
| `WithKeyExtractor(GlobalKey)` | Single breaker for all messages (default). |
| `WithKeyExtractor(SubjectKey)` | Separate breaker per `Envelope.Subject`. |
| `WithKeyExtractor(HeaderKey("tenant-id"))` | Separate breaker per header value. |

### Go Example

The processor wrapper lives in `processors/circuitbreaker`; `Config`, `State`,
and `BreakerMetrics` come from the root `circuitbreaker` state-machine package,
so a program imports both and aliases one:

```go
import (
    cb "github.com/mariotoffia/gobridge/circuitbreaker"
    cbproc "github.com/mariotoffia/gobridge/processors/circuitbreaker"
)

proc := cbproc.New("my-cb", cb.Config{
    FailureThreshold:  10,
    SuccessThreshold:  3,
    ResetTimeout:      60 * time.Second,
    HalfOpenMaxProbes: 2,
},
    cbproc.WithKeyExtractor(cbproc.HeaderKey("tenant-id")),
    cbproc.WithOnStateChange(func(key string, from, to cb.State) {
        slog.Info("breaker state change", "key", key, "from", from, "to", to)
    }),
)
```

Each distinct key gets its own breaker instance, cached per-processor. The cache
is bounded by `WithMaxBreakers` (default 10000) with LRU eviction. All evictions
are counted in `Stats().Evictions`; an eviction that drops an OPEN breaker
additionally increments `Stats().OpenEvictions` (the churn red-flag). Eviction
never drops a busy breaker: entries with in-flight requests are skipped
(in-flight pinning), so a concurrent outcome is never lost to an orphan.
Generation tokens are unrelated to eviction -- they advance only on breaker
**state transitions**, which is what makes a late outcome recorded against a
previous circuit epoch stale (see `StaleOutcomes` below).

**Cluster semantics: per-instance state.** Breaker state is held per bridge
instance and never shared. A fleet of N instances needs up to
N × `FailureThreshold` failures before every breaker has tripped, and instances
open/close out of phase (independent cooldowns) -- the downstream sees
staggered probe traffic from the fleet, not one coordinated circuit.

### Metrics

Retrieve per-key metrics at runtime via `proc.Metrics()`, which returns
`map[string]cb.BreakerMetrics` with fields: `Key`, `State`, `TotalRequests`,
`TotalSuccesses`, `TotalFailures`, `ConsecutiveFailures`, `ConsecutiveSuccesses`,
`StaleOutcomes` (outcomes discarded because their generation token was stale),
and `LastFailureTime`.

---

## Tenant Processor

**Package:** `processors/tenant` | **Config type:** `tenant.Config`

Validates tenant identity and tracks per-tenant usage. Reads the tenant ID
from a configurable header (default: `x-tenant-id`).

### Configuration Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Name` | `string` | `"tenant"` | Unique processor instance name. |
| `TenantHeader` | `string` | `"x-tenant-id"` | Header name containing the tenant ID. A reserved `x-bridge.*` header is rejected. |
| `RequireTenant` | `bool` | `false` | Reject messages that lack a tenant header. |
| `InFlightDecrementTimeout` | `time.Duration` | `2s` | Timeout used to decrement the in-flight counter after a cancelled delivery. |

### Optional Integrations (Go Only)

| Option | Interface | Purpose |
|--------|-----------|---------|
| `WithValidator(v)` | `ports.TenantValidator` | Resolves `TenantInfo`: active check, the `MaxMessageSizeBytes` limit, and the `MaxInFlight` quota ceiling. |
| `WithUsageTracker(t)` | `ports.TenantUsageTracker` | In-flight and processed-message counters. Increment-only, so observational by default; a tracker that also implements `ports.TenantUsageReader` enables the in-flight ceiling (see [Quota enforcement](#quota-enforcement)). |

### Go Example

```go
proc, err := tenant.New(tenant.Config{
    Name:          "my-tenant",
    TenantHeader:  "x-tenant-id",
    RequireTenant: true,
},
    tenant.WithValidator(myTenantValidator),
    tenant.WithUsageTracker(myUsageTracker),
)
```

When a validator is set, the processor rejects inactive tenants and payloads
exceeding `TenantInfo.MaxMessageSizeBytes`. When a usage tracker is set,
in-flight counts are managed around dispatch and successful deliveries increment
the message counter. An increment-only tracker is observational; one that
also implements `ports.TenantUsageReader` enables the in-flight ceiling
described below. Message-count quotas stay unenforced (Phase 2).

### Quota enforcement

The tenant processor can enforce a per-tenant ceiling on concurrent in-flight
deliveries. It stays off unless two conditions hold together:

- **Capability** — the tracker passed to `WithUsageTracker` also implements
  `ports.TenantUsageReader`, the optional read-back extension of
  `ports.TenantUsageTracker`: `Usage(ctx, tenantID) (TenantUsage, error)`,
  returning a `{Messages, InFlight}` snapshot.
- **Data** — the tenant's `TenantInfo.MaxInFlight` is greater than `0` (`0`
  means unlimited, matching the `MaxMessageSizeBytes` convention).

There is no YAML for the ceiling. `TenantInfo`, `MaxInFlight` included, comes
from the embedder's `TenantValidator`, so nothing changes until a validator
returns a ceiling and the tracker implements the reader. An increment-only
tracker, or a zero ceiling, leaves delivery unchanged — the feature is backward
compatible.

When both conditions hold, the processor reads `Usage` before it increments the
in-flight counter. If `InFlight` is at or above `MaxInFlight`, the delivery is
rejected with the transient error `TENANT_QUOTA_EXCEEDED`
(`shared.ErrTenantQuotaExceeded`) and the `TenantRejects` counter records reason
`quota_inflight`. The rejection is transient by design: an over-quota tenant
becomes deliverable again the moment its in-flight count drains, so the route
retry policy — backoff, then DLQ only on exhaustion — is the pressure valve, not
a permanent drop.

**Retry and DLQ tail.** Because the reject is transient, an over-quota delivery
re-enters the route's normal retry pipeline (backoff-driven redelivery), not a
drop. A tenant that stays over its ceiling longer than `MaxReplayAttempts`
backoff cycles has its messages sunk to the DLQ as poison — error code
`POISON_MESSAGE` (`shared.ErrCodePoisonMessage`), DLQ category `max_retries` — so
valid-but-throttled messages are indistinguishable there from genuinely poison
ones. On source transports without delayed-retry support, with a DLQ store
configured, a single quota reject routes straight to the DLQ under category
`retry_unsupported`, with no backoff wait. Size `MaxReplayAttempts` and backoff
for the throttle duration you are willing to tolerate before shedding to the DLQ,
and note that throttled-shed is not currently labeled separately from poison in
the DLQ.

**Fail-open on read error.** When `Usage` returns an error — a usage-store
outage, say — the processor logs a warning, counts it, and proceeds without
enforcing. The quota is a fairness control, not a security boundary; failing
closed would turn a usage-store blip into a full-tenant outage.

**Overshoot bound.** The read and the increment are not atomic, so concurrent
deliveries can each read `InFlight = MaxInFlight - 1` and all pass. The in-flight
counter is per-tenant, but routing is tenant-agnostic — routes match on
subject/binding, not tenant — so a single tenant's traffic spans multiple routes,
each with its own concurrency semaphore, and a shared usage store spans instances
as well. Overshoot is bounded by the tenant's total concurrent in-flight
admissions across every route and instance sharing the usage store, not by one
route's `MaxInFlight`. Acceptable for a fairness quota; exact ceilings would
require a conditional-increment port method, a recorded upgrade path rather than
current behavior.

Windowed message-count quotas (per hour or day) are Phase 2. They need windowed
counter schemas and cross-instance aggregation — the same
per-instance-versus-global question the
[circuit breaker](#circuit-breaker-processor) already answers with per-instance
state.

---

## Store Backends

Three store roles handle different coordination concerns, each configured
independently in the `stores` section.

| Role | Interface | Purpose |
|------|-----------|---------|
| **Lease** | `ports.LeaseStore` | Distributed lease acquisition for exclusive sessions. |
| **Outbox** | `ports.OutboxStore` | Durable outbox for at-least-once delivery with shared sessions. |
| **DLQ** | `ports.DLQStore` | Dead-letter queue for permanently failed messages. |

### YAML Structure

```yaml
stores:
  lease:
    type: memory
  outbox:
    type: sqlite
    options:
      path: /data/outbox.db
  dlq:
    type: dynamodb
    options:
      table_name: my-dlq-table
```

### Memory Store

- **Type:** `memory`
- **Options:** none
- In-process only. Not distributed. Data lost on restart.
- Suitable for development and single-instance setups without durability needs.

### SQLite Store

- **Type:** `sqlite`
- **Required option:** `path` (string) -- file path for the database
- **Optional options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `stale_claim_duration` | duration | runtime-derived | Outbox only -- same semantics as the DynamoDB row below. |
| `retention` | duration | `1h` | Outbox only -- window completed/expired rows are kept before piggybacked compaction deletes them. Negative disables compaction. Keep above any upstream redelivery window. |

- Persistent across restarts. Single-instance only.
- Suitable for single-instance production with disk durability.
- WAL journalling is always on; a single writer connection plus `busy_timeout` serialises in-process writers safely.
- Outbox honours the runtime-derived `stale_claim_duration`: a claim stranded by a crashed owner is reclaimed once it goes stale (in addition to immediate higher-version reclaim). Pair it with a durable lease store for strict multi-restart crash recovery; `memory` lease resets its fencing version on restart.
- No SQLite lease store exists -- use memory or DynamoDB for leases.

### DynamoDB Store

- **Type:** `dynamodb`
- **Options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `table_name` | `string` | `"gobridge-leases"` / `"gobridge-outbox"` / `"gobridge-dlq"` | DynamoDB table name (per role). |
| `stale_claim_duration` | `string` (duration) | runtime-derived (outbox only) | How long before an uncompleted outbox claim is reclaimed. Explicit value wins; when omitted, derived as `maxStepDownGrace + max(2 × maxStepDownGrace, 15s)` across all sessions (see [Configuration Reference](configuration-reference.md)). Failover reclaim via a higher fencing version is always immediate. |
| `compaction_grace` | duration | store default (`1h`) | Outbox only -- window terminal items are kept before the DynamoDB item TTL deletes them. |
| `retention` | duration | none (keep forever) | DLQ only -- TTL on dead-letter entries (`failed_at + retention`). |
| `max_scan_pages` | int | `100` | DLQ only -- bounds index-less List/Purge scans; negative disables. |

- Distributed and persistent. Uses conditional writes for fencing safety.
- Required for clustered deployments with lease-based coordination.
- **Retention is the deduplication window.** The outbox keeps completed and
  expired rows for `retention` / `compaction_grace` before piggybacked compaction
  (or the DynamoDB item TTL) deletes them. Deleting a terminal row releases its
  duplicate-detection identity, so retention IS the duplicate-suppression cover:
  shrinking it shrinks how far back the outbox can suppress a redelivered
  message. Keep it comfortably above any upstream redelivery window.

> **Embedder-only knobs.** Some store options are programmatic by design and
> have no YAML key: `WithClock`, `WithLogger` (all stores); `WithMetrics`,
> `WithCompleteResolveRetry` (dynamodboutbox); `WithGracePeriod` (deprecated
> alias of `WithRetention`, both DynamoDB stores); and every
> `memoryoutbox`/`memorydlq` option (dev store). `WithMaxReplayCount` no longer
> exists -- the store-side replay cap was removed; poison detection is
> drainer-owned per the `ports.OutboxStore` contract.

### Decision Table

| Scenario | Lease | Outbox | DLQ |
|----------|-------|--------|-----|
| Development / testing | `memory` | `memory` | `memory` |
| Production, single instance | -- | `sqlite` | `sqlite` |
| Production, clustered | `dynamodb` | `dynamodb` | `dynamodb` |

**Important:** In clustered deployment mode, the builder rejects non-distributed
stores for all configured roles. Memory and SQLite stores are process-local and
will fail validation when `deployment_mode: clustered` is set.

---

## Programmatic Registration

Register processors and store factories on the `bridge.Builder` before
calling `Build()`:

```go
builder := bridge.NewBuilder(cfg, bridge.WithLogger(logger))

// Register processors
filterProc, _ := filter.New(filter.Config{
    Name: "my-filter", Action: filter.ActionDrop,
    Conditions: []filter.Condition{
        {Field: "header.x-env", Operator: filter.OperatorEquals, Value: "staging"},
    },
})
builder.RegisterProcessor("my-filter", filterProc)

transformProc, _ := transform.New(transform.Config{
    Name: "my-transform",
    Mappings: []transform.FieldMapping{transform.SimpleMapping("$.user.name", "username")},
})
builder.RegisterProcessor("my-transform", transformProc)

cbProc := cbproc.New("my-cb", cb.Config{
    FailureThreshold: 10, ResetTimeout: 60 * time.Second,
}, cbproc.WithKeyExtractor(cbproc.SubjectKey))
builder.RegisterProcessor("my-cb", cbProc)

tenantProc, _ := tenant.New(tenant.Config{Name: "my-tenant", RequireTenant: true})
builder.RegisterProcessor("my-tenant", tenantProc)

// Register store factories
builder.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
builder.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
builder.RegisterStoreFactory("dynamodb", awsstore.NewDynamoDBStoreFactory(ddbClient))

rt, err := builder.Build(ctx)
```

Processor names in the YAML `processors` list must match the names passed to
`RegisterProcessor`. If a name is not found, the builder returns an error.
