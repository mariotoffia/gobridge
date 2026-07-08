# GoBridge — Domain Model (DDD)

The GoBridge domain layer (`domain/`, Clean Architecture Layer 1) is split into six bounded contexts plus a cross-context `events` facts package. Each context owns its primitives, invariants, and ubiquitous-language terms; cross-context coupling is allow-listed in `.go-arch-lint.yml` and re-checked at file scope by `.golangci.yml` `depguard`.

For term definitions see [UBIQUITOUS.md](UBIQUITOUS.md).

---

## 1. Bounded contexts at a glance

| Context | Package | Subdomain | Purpose |
|---|---|---|---|
| Shared Kernel | `domain/shared` | Supporting | Errors (`BridgeError`, `ErrorClass`, `ErrorCode`), metric/tag taxonomy. Imported by every other context. |
| Messaging | `domain/messaging` | **Core** | The canonical `Envelope`, reserved header vocabulary (`x-bridge.*`), W3C Trace Context. |
| Persistence | `domain/persistence` | **Core** | `OutboxRecord` state machine (reliable egress), `LeaseInfo`+`LeaseToken` (cluster fencing), `DrainStrategy` polling policy. |
| Routing | `domain/routing` | **Core** | `RoutePolicy`, `DestinationBinding`, `DispatchPlan`, `DLQEntry` — route decisions and dead-letter records. |
| Connectivity | `domain/connectivity` | Supporting | `CredentialSet` (Password + TLS), `SessionPlan` (desired Subscriptions/Publishers). |
| Events | `domain/events` | Generic | Past-tense observability **facts** about the persistence, routing, connectivity, and configuration contexts (outbox lifecycle, lease fencing, DLQ ingress/redrive, credential rotation, blueprint commit). Constructed by application/runtime services via public constructors (e.g. `NewOutboxRecordClaimed`) **after** a transition succeeds and published through `ports.EventPublisher` — **not** raised by aggregates. Carry primitive payload fields only, so consumers deserialise without importing a producer context. |
| Clock | `domain/clock` | Generic | Time abstraction (`Clock`, `Timer`, `Ticker`). Stdlib-only, no `Sleep` by design — every wait must be cancellable. |

All seven `domain/` packages are stdlib-only at runtime (the `_no_external_deps_` sentinel in `.go-arch-lint.yml`). `domain/events` is the one package that owns no aggregate — it carries facts the application/runtime emit about the other contexts (see `domain/events/doc.go`).

In addition to the seven Layer-1 `domain/` packages above, GoBridge has a **Blueprint / Configuration** *supporting* subdomain that lives at Layer 2 (in `ports/blueprint*.go` plus the `config/` parser/store) and represents the parsed-but-not-yet-built shape of a bridge:

| Context | Package | Subdomain | Purpose |
|---|---|---|---|
| Blueprint / Configuration | `ports` (types) + `config` (parser/store) | Supporting (Layer 2) | The declarative bridge shape: `BridgeConfig`, `BridgeSettings`, `SessionDef`, `BindingDef`, `RouteDef`, `BlueprintValidationError`, `ConfigStore`. Consumed by `bridge.Builder` and the admin `httpapi`. The `ports/` blueprint types are **schema-tagged DTOs** — they carry yaml/json struct *tags* but the `ports` package has no yaml/json runtime *dependency*; the YAML/JSON parser, validator, merger and on-disk manager live in `config/`. |

The Blueprint context is a **Customer/Supplier** consumer of the Routing, Connectivity, and Persistence cores — it *describes* their runtime composition without owning their invariants. See §5 for the context-map edges.

---

## 2. Context map — who imports whom

```mermaid
flowchart TB
    classDef core fill:#fde68a,stroke:#b45309,color:#111
    classDef supporting fill:#bfdbfe,stroke:#1e40af,color:#111
    classDef generic fill:#e5e7eb,stroke:#374151,color:#111

    shared["domain/shared<br/><i>Shared Kernel</i><br/>BridgeError, Metric/Tag"]:::supporting
    messaging["domain/messaging<br/><b>Core</b><br/>Envelope, Headers, TraceContext"]:::core
    persistence["domain/persistence<br/><b>Core</b><br/>OutboxRecord, Lease, DrainStrategy"]:::core
    routing["domain/routing<br/><b>Core</b><br/>RoutePolicy, DLQEntry, DispatchPlan"]:::core
    connectivity["domain/connectivity<br/><i>Supporting</i><br/>CredentialSet, SessionPlan"]:::supporting
    clock["domain/clock<br/><i>Generic</i><br/>Clock, Timer, Ticker"]:::generic

    persistence -->|imports| shared
    persistence -->|imports<br/>OutboxRecord embeds Envelope| messaging
    routing -->|imports| shared
    routing -->|imports<br/>DLQEntry embeds Envelope| messaging
    connectivity -->|imports| shared
    messaging -->|stdlib only| stdlib((stdlib))
    shared -->|stdlib only| stdlib
    clock -->|stdlib only| stdlib
```

The allowed-import edges above are the domain-context rules. Their machine-readable definitions and per-file `depguard` enforcement are documented in [LINT.md](LINT.md); the layer-level rules table lives in [ARCHITECTURE.md §2](ARCHITECTURE.md#2-hexagonal-architecture-layers). `persistence → messaging` is the one cross-context edge — `OutboxRecord.Envelope` is a `messaging.Envelope` value, so an outbox record IS a persisted envelope.

---

## 3. Aggregates, entities, value objects per context

### 3.1 `domain/shared` — Shared Kernel

```mermaid
classDiagram
    class BridgeError {
        +Code: ErrorCode
        +Class: ErrorClass
        +Message: string
        +Cause: error
        +RetryAfter: time.Duration
        +Context: map~string,any~
        +Error() string
        +Unwrap() error
        +Is(target) bool
        +With(k,v) BridgeError
        +WithMessage(m) BridgeError
        +Wrap(err) BridgeError
        +WithRetryAfter(d) BridgeError
    }
    class ErrorClass {
        <<value object / enum>>
        transient | permanent | expired | rejected
    }
    class ErrorCode {
        <<value object / enum>>
        TIMEOUT, NOT_AUTHORIZED, ...
    }
    class Tag {
        <<value object>>
        +Key: string
        +Value: string
    }
    BridgeError --> ErrorClass
    BridgeError --> ErrorCode
```

- **`BridgeError`** — structured error carrying classification (`Class`) and identity (`Code`). Treated as a copy-on-write value: `With*` methods return a clone. Used by every adapter and runtime component to drive retry / DLQ / drop decisions.
- **`ErrorClass`** — drives pipeline routing:
  `transient` → retry, `permanent` → DLQ, `expired` → expired-action policy, `rejected` → drop.
- **`ErrorCode`** — sentinel identity for `errors.Is` comparisons.
- Metric and tag-key constants — single source of truth for the metric taxonomy.

### 3.2 `domain/messaging` — Core (the canonical message)

```mermaid
classDiagram
    class Envelope {
        <<aggregate root>>
        +ID: string
        +Subject: string
        +Payload: []byte
        +Headers: map~string,any~
        +CreatedAt: time.Time
        +ExpiresAt: time.Time
        +HasExpiry() bool
        +IsExpired(clk) bool
        +RemainingTTL(clk) Duration
        +Clone() *Envelope
    }
    class TraceContext {
        <<value object>>
        +TraceID, SpanID, State: string
        +Flags: byte
    }
    class Headers {
        <<utilities>>
        IsReservedHeader(k)
        StripReservedHeaders(h)
        MergeHeaders(base, overlay, protectReserved)
        Get/SetHeader(...)
    }
    Envelope --> Headers : reserved-prefix invariants
    Envelope --> TraceContext : Extract/Inject via headers
```

- **`Envelope`** — aggregate root for the messaging context. Invariants:
  - `Clone()` deep-copies `Headers` and `Payload` so reference values are never shared between original and clone.
  - Expiry checks must take a `Clock` (no implicit `time.Now()`).
  - `Subject` is the **logical event subject**. It is producer-supplied (or ingress-mapped from a transport-native subject field, e.g. `Message.Properties.Subject`, the `gobridge.subject` carrier on MQTT/AMQP 0-9-1, or the HTTP JSON `subject`). The runtime never assigns a transport destination to `Subject`; per-message destinations travel on `ports.OutboundMessage.Address` (see § 3.4).
- **Reserved header invariant** — keys with prefix `x-bridge.` are bridge-internal. `IsReservedHeader` is case-insensitive; transport adapters must `StripReservedHeaders` at ingress to prevent injection. `MergeHeaders(..., protectReserved=true)` denies overlay overrides of reserved keys already in base.
- **`TraceContext`** — W3C Trace Context VO. `ParseTraceparent` rejects non-`00` versions, all-zero IDs, wrong lengths, non-lowercase hex.

### 3.3 `domain/persistence` — Core (reliable egress + cluster fencing)

```mermaid
classDiagram
    class OutboxRecord {
        <<aggregate root>>
        +ID, RouteID, EnvelopeID, BindingID, SessionID, Address: string
        +Envelope: messaging.Envelope
        +DispatchHeaders: map
        +Status: OutboxStatus
        +ClaimedBy: string
        +ClaimedAt, CreatedAt, ExpiresAt, CompletedAt: time.Time
        +ClaimVersion: uint64
        +ReplayCount: int
    }
    class OutboxStatus {
        <<state>>
        pending → claimed → completed
                       ↘ expired
    }
    class LeaseInfo {
        <<read-only snapshot / value object>>
        +LeaseID, Owner: string
        +Version: uint64
        +ExpiresAt: time.Time
        +Endpoints: map~string,string~
    }
    class LeaseToken {
        <<value object / fencing>>
        +Version: uint64
        +Owner: string
    }
    class PeerInfo {
        <<value object>>
        +InstanceID: string
        +Endpoints: map
    }
    class DrainStrategy {
        <<domain service / policy>>
        NextInterval(recordsFound) Duration
    }
    class FixedPoll
    class AdaptiveBackoff
    DrainStrategy <|.. FixedPoll
    DrainStrategy <|.. AdaptiveBackoff
    OutboxRecord --> OutboxStatus
    LeaseInfo ..> LeaseToken : returned alongside
```

**Invariants:**

- `OutboxRecord.Status` follows the state machine `pending → claimed → completed | expired`. Only the lease holder identified by `LeaseToken{Owner, Version}` may claim; `ClaimVersion` enforces fencing on optimistic update.
- `OutboxPartitionKey(sessionID, bindingID)` is the canonical key — `SESSION#<id>` if session is set, else `BINDING#<id>`.
- `LeaseInfo` is a read-only snapshot DTO with no behavior; `LeaseStore` owns the lease lifecycle and fencing invariants, enforcing them via conditional (compare-and-set) writes keyed on `LeaseToken.Version`. `LeaseStore` keeps `Version` monotonic and returns `shared.ErrCodeStaleFencingToken` on stale-token writes.
- `DrainStrategy.NextInterval` is the **only** way the drainer learns its next wait. `FixedPoll` returns a constant ±25 % jitter; `AdaptiveBackoff` resets to `MinInterval` when records were found, else multiplies up to `MaxInterval`. `AdaptiveBackoff` is single-goroutine.

### 3.4 `domain/routing` — Core (route decisions + DLQ)

```mermaid
classDiagram
    direction TB
    class RoutePolicy {
        <<aggregate root>>
        +MaxInFlight, MaxReplayAttempts, MaxOutboxDepth: int
        +Backoff: BackoffPolicy
        +OnExpired: ExpiredAction
        +OnPermanentFailure: FailureAction
        +DeliveryMode: DeliveryMode
        +DispatchMode: DispatchMode
        +AckAfter: AckBoundary
        +AllowUnfenced, AllowRetryDrop: bool
        +SendTimeout, DepthCacheTTL, ProcessorTimeout: Duration
        +WithDefaults() RoutePolicy
    }
    class BackoffPolicy {
        <<value object>>
        +InitialInterval, MaxInterval: Duration
        +Multiplier: float64
    }
    class DestinationBinding {
        <<entity>>
        +ID, Transport, SessionID, SenderID, Address: string
        +Config: any
        +Headers: map
    }
    class DispatchPlan {
        <<value object>>
        +BindingID, Address: string
        +Headers: map
    }
    class DLQEntry {
        <<entity>>
        +ID, RouteID, BindingID, SessionID, SourceID,
         CorrelationID, Reason, Category, ErrorCode, LastError: string
        +Envelope: messaging.Envelope
        +Attempts: int
        +FailedAt: time.Time
    }
    class DLQFilter {
        <<value object>>
        +RouteID, Category: string
        +Since, Before: Time
        +Limit: int
    }
    RoutePolicy --> BackoffPolicy
    RoutePolicy --> DeliveryMode
    RoutePolicy --> DispatchMode
    RoutePolicy --> AckBoundary
    RoutePolicy --> ExpiredAction
    RoutePolicy --> FailureAction
    DispatchPlan --> DestinationBinding
    DLQEntry --> DestinationBinding
    DLQFilter ..> DLQEntry
```

**Invariants:**

- `RoutePolicy.WithDefaults()` is the single canonical normalization step — every zero-or-invalid field collapses to a documented default (`DefaultMaxInFlight`, `DefaultSendTimeout`, …). Callers should not hand-fill defaults elsewhere.
- `DeliveryMode` (`direct_hold` | `shared_outbox`), `DispatchMode` (`single` | `fan_out`), `AckBoundary` (`target_accept` | `outbox_persist`) are exhaustive enums.
- `DLQEntry.Envelope` is the immutable snapshot of the envelope at failure; `Attempts` and `FailedAt` describe the failure event.
- `DispatchPlan.Address` is the transport destination for one envelope (publish topic, routing key, queue URL, ...). It is passed to the sender via the `ports.OutboundMessage{Envelope, Address}` port type and is **never** written back into `Envelope.Subject`. The shared-outbox store records `OutboxRecord.Address` alongside the embedded envelope so drainers reconstruct the same `OutboundMessage` shape.

### 3.5 `domain/connectivity` — Supporting (auth + reconciliation shape)

```mermaid
classDiagram
    class CredentialSet {
        <<aggregate root>>
        +Password: *PasswordCredential
        +TLS: *TLSMaterial
        +Equal(other) bool
    }
    class PasswordCredential {
        <<value object>>
        +Username, Password: string
        +String() "REDACTED"
    }
    class TLSMaterial {
        <<value object>>
        +CertPEM, KeyPEM: string
        +CAPEMs: []string
        +InsecureSkipVerify: bool
        +String() "REDACTED"
    }
    class CredentialKind {
        <<enum>>
        password | tls
    }
    class SessionPlan {
        <<aggregate root>>
        +Subscriptions: []SubscriptionPlan
        +Publishers: []PublisherPlan
    }
    class SubscriptionPlan {
        <<value object>>
        +Topic: string
        +QoS: int
        +Config: any
    }
    class PublisherPlan {
        <<value object>>
        +Topic: string
        +QoS: int
        +Config: any
    }
    class SessionMode {
        <<enum>>
        ephemeral | persistent | exclusive
    }
    CredentialSet o-- PasswordCredential
    CredentialSet o-- TLSMaterial
    SessionPlan o-- SubscriptionPlan
    SessionPlan o-- PublisherPlan
```

**Invariants:**

- Credential types redact in `String()` / `GoString()` — the `%v` / `%+v` of a credential **never** discloses material.
- `CredentialSet.Equal` is a deep value comparison; push-credential adapters use it to dedup rotation events so no-change resolves do not emit.
- `SessionPlan` is the **desired** state of a transport session — adapters reconcile actual subscriptions/publishers toward this plan.

### 3.6 `domain/clock` — Generic (time abstraction)

```mermaid
classDiagram
    class Clock {
        <<port>>
        +Now() Time
        +Since(t) Duration
        +NewTimer(d) Timer
        +NewTicker(d) Ticker
        +After(d) chan Time
    }
    class Timer {
        +C() chan Time
        +Reset(d) bool
        +Stop() bool
    }
    class Ticker {
        +C() chan Time
        +Reset(d)
        +Stop()
    }
    class System {
        <<singleton>>
        wraps stdlib time
    }
    Clock <|.. System
```

**Invariant:** `Clock` exposes no `Sleep`. Every wait must be expressed as
`select { case <-ctx.Done(): case <-clk.After(d): }` so cancellability is enforced at the type level. Tests use `domain/clock/clocktest` (excluded from arch lint).

### 3.7 Blueprint / Configuration — Supporting (Layer 2)

Lives in `ports/blueprint*.go` (schema-tagged DTOs — yaml/json struct tags, no yaml/json runtime dep) and `config/`
(YAML/JSON parser, validator, merger, on-disk store). Not a Layer-1
domain context — it is the *application-layer* description of what a
bridge looks like before `bridge.Builder` materialises it into running
adapters.

```mermaid
classDiagram
    class BridgeConfig {
        <<aggregate root>>
        +Version int
        +Bridge BridgeSettings
        +ConfigWatch *ConfigWatchDef
        +Stores StoresConfig
        +Sessions []SessionDef
        +Receivers []ReceiverDef
        +Senders []SenderDef
        +Bindings []BindingDef
        +Routes []RouteDef
        +HTTP *HTTPConfig
    }
    class BridgeSettings {
        +ID string
        +InstanceID string
        +DeploymentMode string
        +ShutdownTimeout string
        +DrainTimeout string
        +Cluster *ClusterConfig
    }
    class SessionDef {
        +ID string
        +Transport string
        +Mode SessionMode
        +Config any  %% typed PluginConfig
    }
    class BindingDef {
        +ID string
        +Transport string
        +SessionID string
        +SenderID string
        +Address string
        +Config any  %% typed PluginConfig
    }
    class RouteDef {
        +ID string
        +ReceiverID string
        +DeliveryMode DeliveryMode
        +DispatchMode DispatchMode
        +Bindings []string
        +Policy RoutePolicy
    }
    class BlueprintValidationError {
        +Errors []string
        +Warnings []string
        +Add(msg)
        +Addf(fmt, args)
        +Warnf(fmt, args)
        +HasErrors() bool
    }
    class ConfigStore {
        <<port>>
        +Load() *BridgeConfig
        +Save(*BridgeConfig) error
        +Validate(*BridgeConfig) (warnings, error)
        +Merge(base, overlay) *BridgeConfig
    }

    BridgeConfig "1" --> "1" BridgeSettings
    BridgeConfig "1" --> "*" SessionDef
    BridgeConfig "1" --> "*" BindingDef
    BridgeConfig "1" --> "*" RouteDef
    ConfigStore ..> BridgeConfig : load / save / merge
    ConfigStore ..> BlueprintValidationError : validate
```

**Invariants:**

- `BridgeConfig` is the aggregate root of the blueprint. `bridge.Builder` consumes it whole; partial / mutated copies are not valid inputs.
- The blueprint is **dependency-neutral, not tag-free** — the `ports/` blueprint structs carry yaml/json struct *tags* (they are schema-tagged DTOs by design), but the `ports` package itself has no yaml/json/mapstructure runtime *dependency*, so the inner ring stays dependency-neutral. The `config/` package owns parsing, defaulting, merging and on-disk transactions; `httpapi` interacts with it only through the `ConfigStore` port.
- `BlueprintValidationError` separates hard `Errors` (block startup / commit) from advisory `Warnings` (surface in admin UI but allow). The admin layer consumes both without depending on the parser package.
- Plugin-specific configuration (transport options, store driver options, processor params) is carried as typed `ports.PluginConfig` values inside `SessionDef.Config`, `BindingDef.Config`, `StoreConfig.Config`, etc. — never as `map[string]any`. See [PLUGIN.md §Typed Plugin Config](PLUGIN.md#typed-plugin-config) and `cfgshape` enforcement.
- `BridgeConfig.Version` is an optimistic-concurrency counter; `ConfigStore.Save` must perform a check-and-set against the version the transaction was started with (so concurrent operators on a shared file/DB cannot silently overwrite).

---

## 4. Outer-ring consumers (who depends on the domain)

Layer-by-layer dependencies (composition root → adapters → application → ports → domain), the full per-component allowed-imports table, and the allow-listed inward edges from outer rings live in [ARCHITECTURE.md §2 Hexagonal Architecture Layers](ARCHITECTURE.md#2-hexagonal-architecture-layers). They are part of the runtime layering, not the domain model — kept out of this file so DDD.md stays bounded-context-shaped.

---

## 5. Cross-context relationships (context-map style)

```mermaid
flowchart LR
    classDef core fill:#fde68a,stroke:#b45309,color:#111
    classDef supporting fill:#bfdbfe,stroke:#1e40af,color:#111
    classDef generic fill:#e5e7eb,stroke:#374151,color:#111

    shared["shared<br/><i>Shared Kernel (S)</i>"]:::supporting
    messaging["messaging<br/><b>Core</b>"]:::core
    persistence["persistence<br/><b>Core</b>"]:::core
    routing["routing<br/><b>Core</b>"]:::core
    connectivity["connectivity<br/><i>Supporting</i>"]:::supporting
    clock["clock<br/><i>Generic</i>"]:::generic

    shared -- "Shared Kernel (errors, metrics)" --> messaging
    shared -- "Shared Kernel" --> persistence
    shared -- "Shared Kernel" --> routing
    shared -- "Shared Kernel" --> connectivity
    messaging -- "Customer / Supplier<br/>OutboxRecord embeds Envelope" --> persistence
    messaging -- "Customer / Supplier<br/>DLQEntry embeds Envelope" --> routing
    blueprint["blueprint<br/><i>Supporting (L2)</i><br/>BridgeConfig, *Def, ConfigStore"]:::supporting
    blueprint -- "Customer / Supplier<br/>describes routes" --> routing
    blueprint -- "Customer / Supplier<br/>describes sessions/credentials" --> connectivity
    blueprint -- "Customer / Supplier<br/>describes stores/outbox" --> persistence
```

- **Messaging is upstream** of persistence and routing. Persistence and routing model "an `Envelope` that has been persisted / has failed routing". Messaging is the publisher of the canonical message shape; persistence and routing are consumers (Customer/Supplier in DDD terms).
- **Shared is the Shared Kernel** — every other context depends on its error class/code taxonomy. Changes to `BridgeError` ripple everywhere; this is intentional and reviewed.
- **Connectivity, Routing, and Persistence do not know about each other.** They communicate only by exchanging `messaging.Envelope` and `shared.BridgeError` values through ports defined in Layer 2.
- **Blueprint (Layer 2)** is *upstream* in description but *downstream* in dependency: `bridge.Builder` consumes a `BridgeConfig` and produces concrete adapters for routing, connectivity, and persistence. The blueprint context defines no domain invariants of its own — it borrows the vocabulary of the cores it composes (Customer/Supplier).

---

## 6. Invariant summary (one line each)

| Invariant | Owner | Enforced by |
|---|---|---|
| Reserved-prefix headers (`x-bridge.*`) cannot be set by external sources at ingress | messaging | `StripReservedHeaders`, `MergeHeaders(protectReserved=true)` |
| `Envelope.Clone()` deep-copies headers and payload | messaging | `Clone` + `audit_deep_copy_test.go` |
| `traceparent` rejects non-`00`, all-zero IDs, non-low-hex | messaging | `ParseTraceparent` + `audit_tracecontext_test.go` |
| Outbox state machine: `pending → claimed → completed \| expired` | persistence | `OutboxStatus` constants + adapter contract tests |
| Lease fencing: only holder of current `LeaseToken{Owner,Version}` may write | persistence | `ClaimVersion` optimistic check, `ErrCodeStaleFencingToken` |
| Drain wait is always cancellable | persistence + clock | `DrainStrategy.NextInterval` + `Clock.After` |
| `RoutePolicy` zero-fields collapse to documented defaults | routing | `RoutePolicy.WithDefaults()` |
| Credentials redact in `String`/`GoString` | connectivity | `PasswordCredential.String`, `TLSMaterial.String` |
| `CredentialSet.Equal` is value-based for push-rotation dedup | connectivity | `CredentialSet.Equal` + tests |
| Time waits are cancellable; no `Sleep` exists | clock | `Clock` interface (no `Sleep` method) |
| Errors are classified by `ErrorClass` to drive retry / DLQ / drop | shared | `BridgeError.Class`, runtime classifier |

---

## 7. Where to read more

- **Architectural rules and lint:** `.go-arch-lint.yml`, `.golangci.yml` (depguard rules per context), `scripts/lint-arch-mapping-test.sh`.
- **Layer overview:** `ARCHITECTURE.md`.
- **Term glossary:** `UBIQUITOUS.md`.
- **Source of truth:** the `domain/<context>/doc.go` files describe each context in one paragraph at the top of the package.
