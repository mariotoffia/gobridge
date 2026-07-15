# GoBridge — Ubiquitous Language

Authoritative glossary. One term per row. If a concept appears in code, config, logs, or docs, it must use this name and this meaning.

Grouped by bounded context (see [DDD.md](DDD.md)).

---

## Shared kernel (`domain/shared`)

| Term | Meaning |
|---|---|
| **BridgeError** | Structured error returned across every layer. Carries `Code`, `Class`, `Message`, optional `Cause`, `RetryAfter`, free-form `Context`. |
| **ErrorClass** | Classification that drives pipeline routing: `transient` (retry), `permanent` (DLQ), `expired` (run expired-action policy), `rejected` (drop, no DLQ). |
| **ErrorCode** | Sentinel identity for `errors.Is` (e.g. `TIMEOUT`, `NOT_AUTHORIZED`, `STALE_FENCING_TOKEN`). |
| **RetryAfter** | Hint on a transient error telling the runtime how long to wait before retrying. |
| **Tag** | Key-value dimension attached to an emitted metric. |
| **MetricNamespace** | Single namespace `GoBridge/Runtime` under which every bridge metric is reported. |
| **instance_id tag** | Metric dimension (`TagKeyInstanceID = "instance_id"`) added by the CloudWatch exporter's `WithInstanceTag` so per-instance series in a fleet do not collide. Sourced from `BridgeSettings.InstanceID`; never applied to rollup-metric copies. |
| **Exporter self-metrics** | The CloudWatch exporter's own health counters: `ExporterDroppedDatums` (datums dropped under buffer pressure) and `ExporterRejectedDatums` (datums CloudWatch rejected on `PutMetricData`). |
| **MetricRouteRestarts** | Metric `RouteRestarts` (`domain/shared`) counting per-route supervised restarts: a route runner returned an error and was restarted in isolation under jittered capped backoff instead of tearing down the runtime. Tagged `route_id`. Mirrors `SessionRestarts` for the ingress/route side; alert on the rate, not on liveness. |
| **ErrBrokerBusy** | Transient error `shared.ErrBrokerBusy` (code `BROKER_BUSY`) raised when a broker has flow control engaged. The amqp091 sender returns it to fail fast on a `connection.blocked` resource alarm instead of wedging every publisher on a publish the SDK cannot cancel. |
| **MetricCredentialRotationApplied** | Metric `CredentialRotationApplied` (`domain/shared`) counting credential rotations applied to a live transport — one per target whose `ApplyCredentials` returned without error (`bridge.CredentialRefresher`). Success counterpart to `CredentialRefreshFailures`; the URI is never a dimension because it may carry secrets. |
| **MetricCredentialResolveFailure** | Metric `CredentialResolveFailure` (`domain/shared`) counting credential repository fetch failures at the resolver choke point (`runtime.CredentialResolver`), tagged with the bounded error `code` (e.g. `NOT_AUTHORIZED`, `UNAVAILABLE`, `NOT_FOUND`). Covers build-time resolves, rotation polls, and reactive re-resolves. |
| **MetricCredentialStaleServed** | Metric `CredentialStaleServed` (`domain/shared`) counting stale-while-error serves: the resolver returned an expired but last-known-good `CredentialSet` after a retryable fetch error, tagged with the error `code`. Never emitted for permanent errors. |

## Messaging (`domain/messaging`)

| Term | Meaning |
|---|---|
| **Envelope** | The canonical message moving through the bridge. Carries `Subject`, `Headers`, and the immutable fields `ID`, `Payload`, `CreatedAt`, `ExpiresAt` — accessed via read-only accessors (`ID()`, `Payload()`, `CreatedAt()`, `ExpiresAt()`); constructed via `NewEnvelope`/`MustEnvelope`. Machine-enforced aggregate root of the messaging context. |
| **Subject** | Logical event subject of an envelope. Free-form string set by the producer (or by an ingress mapping). The runtime never overwrites it with a transport destination. Distinct from `Address`. |
| **Address** | Transport destination chosen at egress time for one envelope: a publish topic, AMQP routing key, queue URL, etc. Lives on `DestinationBinding.Address`, `DispatchPlan.Address`, `OutboxRecord.Address`, and `ports.OutboundMessage.Address`. Never written into `Envelope.Subject`. |
| **OutboundMessage** | The unit `Sender.Send`/`BatchSender.SendBatch` consume: `{Envelope *domain.Envelope, Address string}`. Carries the logical envelope and the resolved transport destination side-by-side. |
| **`gobridge.subject`** | Cross-transport carrier for `Envelope.Subject` on transports without a native subject field (MQTT user property, AMQP 0-9-1 header). Application-visible, not reserved. |
| **Reserved header** | Header key with prefix `x-bridge.` (case-insensitive). Bridge-internal; must be stripped from external sources at ingress. Two classes: internal-only (route-id, route-override, source-id, content-type — stripped always) and bridge-to-bridge propagated (correlation-id, causation-id, idempotency-key, dedup-id, ordering-key, tenant-id, forwarded-from/hop — preserved only when `TrustBridgeHeaders` is set). |
| **TrustBridgeHeaders** | Opt-in `RoutePolicy` flag (`trust_bridge_headers` on `RouteDef`). When set, ingress preserves the bridge-to-bridge propagated header class instead of stripping it. Internal-only headers are stripped regardless. Enable only on receivers fed exclusively by a trusted upstream bridge — blanket trust would let external producers spoof e.g. tenant identity. |
| **Correlation ID** | `x-bridge.correlation-id`. End-to-end ID used to correlate logs/traces across hops. |
| **Causation ID** | `x-bridge.causation-id`. ID of the envelope that caused this one to be produced. |
| **Idempotency key** | `x-bridge.idempotency-key`. Used by adapters and outbox to dedup. |
| **Ordering key** | `x-bridge.ordering-key`. Hint to ordered transports (e.g. SQS FIFO group). |
| **Dedup ID** | `x-bridge.dedup-id`. Transport-level deduplication identifier. |
| **Forwarded-from / Forwarded-hop** | Cluster-forwarding lineage headers. |
| **`x-bridge.retry-attempt`** | Reserved Service Bus header carrying the receive/attempt count across a delayed (scheduled) retry. Written on outbound and read back on inbound by the ASB adapter. |
| **`x-bridge.original-message-id`** | Reserved Service Bus header preserving the first-attempt `MessageID` across scheduled retries so end-to-end dedup survives re-enqueue. |
| **TraceContext** | W3C Trace Context (`traceparent` + `tracestate`). Parsed/formatted via the helpers in `messaging` (the domain path). The `adapters/otel/tracing` adapter additionally bridges the same lowercase `traceparent`/`tracestate` keys into the OpenTelemetry span context via the OTel `propagation.TraceContext` propagator — it may not import `domain/messaging` (`.go-arch-lint.yml`) yet round-trips the identical wire format. |

## Persistence (`domain/persistence`)

| Term | Meaning |
|---|---|
| **Outbox** | Durable store of envelopes awaiting reliable egress. The transactional pattern that decouples "accepted from source" from "sent to target". |
| **OutboxRecord** | One persisted envelope plus dispatch metadata, status, claim ownership, and replay counters. Aggregate root. |
| **OutboxStatus** | State of an `OutboxRecord`: `pending` → `claimed` → `completed` (or → `expired`). |
| **OutboxPartitionKey** | Canonical partition key derived from `(sessionID, bindingID)`: `SESSION#<id>` if session set, else `BINDING#<id>`. |
| **Drain** | The act of pulling pending records from the outbox and dispatching them. |
| **DrainStrategy** | Policy returning the next wait between drain cycles. Implementations: `FixedPoll`, `AdaptiveBackoff`. |
| **AdaptiveBackoff** | Drain policy that resets to `MinInterval` when records were found, else exponentially backs off up to `MaxInterval` (with ±25 % jitter). |
| **Lease** | Cluster ownership grant. The current owner alone may claim/complete outbox records and forward routes. |
| **LeaseInfo** | Read-only snapshot of lease state (`LeaseStore` owns the invariants): `LeaseID`, `Owner`, `Version`, `ExpiresAt`, `Endpoints`. |
| **LeaseToken** | Fencing token `{Owner, Version}` returned with the lease. Carried on every guarded write so stale owners are rejected. |
| **Fencing token / Fencing** | The mechanism that uses `LeaseToken.Version` (monotonic) to reject writes from a previous owner after a lease transfer. |
| **Stale fencing token** | A write attempted with an older `Version` than the current lease. Rejected with `shared.ErrCodeStaleFencingToken`. |
| **Replay** | Re-attempting outbox dispatch of a previously claimed record. Counted by `ReplayCount`, capped by `RoutePolicy.MaxReplayAttempts`. |
| **PeerInfo** | Remote bridge instance discovered via lease ownership history. |
| **Seq** | Monotonic per-partition persistence sequence the store assigns at persist time (`OutboxRecord.Seq()`). Outbox claim ordering is `(CreatedAt, Seq)`; `Seq` breaks ties within the same `CreatedAt`. |
| **PoisonMinAge** | Minimum wall-clock record age (from `CreatedAt`), required in addition to exceeding `RoutePolicy.MaxReplayAttempts`, before the outbox drainer routes a record to the DLQ as poison. Guards against an outage DLQ-ing an otherwise-good record. |
| **Replay budget** | Wall-clock budget, measured from a record's first delivery attempt (`FirstAttemptedAt`), that bounds total redelivery time before the outbox drainer poisons the record to the DLQ. Policy field `RoutePolicy.ReplayBudget` (YAML `replay_budget`), default 15m (`DefaultReplayBudget`). Distinct from `MaxReplayAttempts`, now the minimum-attempts floor: poison requires the floor exceeded, a non-zero first attempt, and the budget elapsed, together. |
| **First attempt timestamp** | `OutboxRecord.FirstAttemptedAt`: the instant the drainer first claimed the record, stamped once and never moved by a later release or reclaim. Proves a delivery attempt was made, so it is the clock the replay budget runs from. Zero for records persisted before the first-attempt schema or never yet claimed; those fall back to the `CreatedAt` age gate (`PoisonMinAge`). |

## Routing (`domain/routing`)

| Term | Meaning |
|---|---|
| **Route** | A logical edge from a source binding through processors to one or more destination bindings, governed by a `RoutePolicy`. |
| **RoutePolicy** | Per-route delivery, retry, backpressure, and timeout configuration. Aggregate root for routing. |
| **BackoffPolicy** | Retry backoff parameters: `InitialInterval`, `MaxInterval`, `Multiplier`. |
| **DeliveryMode** | `direct_hold` (hold source ack until target accepts) or `shared_outbox` (persist then ack source). |
| **DispatchMode** | `single` (one binding per envelope) or `fan_out` (every matching binding). |
| **AckBoundary** | `target_accept` (ack source after target accepts) or `outbox_persist` (ack source after outbox persists). With `DeliveryMode`, sets the per-route at-least-once contract — lease/version fencing bounds but does not eliminate the duplicate window (in-flight sends at lease transfer), so downstream consumers dedup on `x-bridge.idempotency-key` / `x-bridge.dedup-id` (see [docs/troubleshooting.md](docs/troubleshooting.md)). |
| **ExpiredAction** | What happens to expired envelopes: `drop` or `dlq`. |
| **FailureAction** | What happens on permanent failure: `dlq` or `drop`. |
| **FilteredAction** | What happens to an intentionally filtered envelope: `drop` (default) or `dlq`. |
| **DestinationBinding** | A concrete target an envelope can be sent to: `Transport`, `SessionID`, `SenderID`, `Address`, plugin `Config`, optional static `Headers`. `Address` is the transport destination, not the logical subject. |
| **DispatchPlan** | Result of resolving destinations for one envelope: which `BindingID`, which `Address` (the per-message transport destination passed to the sender via `OutboundMessage.Address`), merged `Headers`. |
| **DLQ** | Dead-letter queue. Permanent record of envelopes that could not be delivered. |
| **DLQEntry** | One DLQ record: snapshot `Envelope`, route/binding/session/source IDs, `Reason`, `Category`, `ErrorCode`, `LastError`, `Attempts`, `FailedAt`. |
| **DLQFilter** | Query criteria for DLQ scans: by route, category, time window, limit. |
| **MaxInFlight** | Per-route concurrency cap for in-flight dispatches. |
| **MaxOutboxDepth** | Per-route backpressure cap on outbox depth. |
| **DepthCacheTTL** | TTL of the cached outbox-depth value used for fast backpressure checks. |
| **ProcessorTimeout** | Per-processor execution deadline. Exceeding it returns `ErrProcessorTimeout` (transient); panicking returns `ErrProcessorPanic` (permanent). |
| **AllowUnfenced** | Route-level escape hatch permitting writes without a fencing token. Off by default. |
| **AllowRetryDrop** | Route-level flag permitting silent drop instead of DLQ on retry exhaustion. Off by default. |

## Connectivity (`domain/connectivity`)

| Term | Meaning |
|---|---|
| **Session** | A live connection to a transport (one MQTT/AMQP/SQS/ASB/HTTP connection) with subscriptions and publishers attached. |
| **SessionMode** | `ephemeral` (no durable subscriptions), `persistent` (durable, resumed across restarts), `exclusive` (single-owner). |
| **DurableSessionIdentity** | Opaque, secret-safe SHA-256 fingerprints returned by an optional typed plugin-config capability. The state fingerprint covers every effective field selecting transport-owned durable broker state; each ownership-domain fingerprint covers one canonical broker endpoint plus effective client identity. The Supervisor compares them before startup/live reload and never logs raw identity material. |
| **FreezableConfig** | Optional adapter-owned `PluginConfig` capability (`FreezePluginConfig`) producing a deep-owned immutable configuration snapshot across preflight/build/store/rollback boundaries. Mutable configuration is copied by the owning adapter; explicitly opaque runtime dependencies (client handles, clocks, mutex-bearing state, process-stable suffix resolvers) retain identity. Durable identity configs must implement it; core code never reflect-clones opaque plugin values. |
| **ReplicaIdentityStrategy** | Optional typed plugin-config declaration proving how clustered shared consumers obtain unique connection identities. `hostname` is stable and preferred; process-unique `nonce` is valid only for Ephemeral sessions. Exclusive sessions use no suffix and retain one stable shared identity. |
| **SessionPlan** | The desired state of a session: lists of `SubscriptionPlan`, `PublisherPlan`, and sorted/deduplicated `ExpectedReceiverIDs`. Adapters reconcile actual state toward the plan; Full readiness requires exact successful subscription convergence (including stale removals) and every expected receiver handler. An empty expected-ID list preserves compatibility for programmatic legacy plans. |
| **Production Lease TTL floor** | Uniform minimum effective `LeaseTTL` of 5 seconds for production Exclusive-session composition. Validation rejects a lower value before any LeaseStore backend is opened; deterministic direct-manager tests may bypass composition validation to use compressed time. |
| **Post-acquire activation bound** | Conservative hard timeout for the complete sequential `Start` + `Reconcile` path after an Exclusive Lease is acquired, including durable cleanup, recycle, and replay verification. A typed transport exposes the effective whole-path duration through `PostAcquireActivationTimingConfig`; the builder resolves and validates it before opening stores/transports. The existing Lease renewer starts immediately after Acquire and remains the sole renewer throughout activation; lease loss cancels activation and disconnects fail-closed. This bound is distinct from the shorter recurring reconnect-event reconcile cap and is not a failover-SLO claim. |
| **SubscriptionPlan** | Desired subscription: `Topic`, `QoS`, transport-typed `Config`. |
| **ManagedSubscriptionStore** | Connectivity-owned port that persists the exact MQTT topic-filter history for one secret-safe `DurableSessionIdentity`. `List` distinguishes a missing baseline from an explicit empty baseline; `Remember` is write-ahead before `SUBSCRIBE`; `Forget` follows only per-filter successful `UNSUBACK`. Results are sorted/deduplicated and all operations are idempotent. |
| **Managed subscription history** | The durable set of exact filters GoBridge may remove from a persistent/exclusive MQTT broker session. It includes ordinary wildcard filters and the complete `$share/<group>/<filter>` string; it is never inferred from a delivered concrete topic. |
| **PublisherPlan** | Desired publisher: `Topic`, `QoS`, transport-typed `Config`. |
| **Reconcile** | The act of bringing a session's actual subscriptions/publishers in line with its `SessionPlan`. |
| **Credential** | Authentication material. One of: `password`, `tls`. |
| **CredentialSet** | Composite credential container — may carry both a `PasswordCredential` and `TLSMaterial`; accessed via nil-safe accessors `Password()` and `TLS()`; constructed via `NewCredentialSet(password, tls)`. Machine-enforced aggregate root. |
| **PasswordCredential** | `Username` + `Password`. Redacts in `String`/`GoString`. |
| **TLSMaterial** | `CertPEM`, `KeyPEM`, `CAPEMs`, `InsecureSkipVerify`. Redacts in `String`/`GoString`. |
| **Push credential / Pull credential** | Two delivery modes for credential rotation: push stores emit on change; pull stores are polled. `CredentialSet.Equal` is the canonical change check. |
| **Single-use exclusive session** | Invariant that the session-based source transports (paho MQTT, amqp091, amqp10) are single-use: once `Close` runs, `Start` returns a permanent `shared.ErrUnavailable` rather than reconnecting. `CapExclusiveIdentity` is declared only by paho MQTT and amqp091; amqp10 does not advertise it but obeys the same single-use rule when run as an exclusive session. The session-zombie terminal escalation (`ErrSessionUnrecoverable`) and the orchestrator process-restart backstop rely on it. |
| **ErrSessionUnrecoverable** | `session.ErrSessionUnrecoverable` (`runtime/session`): a lease-owning term failed because a single-use exclusive session cannot be re-`Start`ed in this process after a step-down `Close` (Start-after-Close surfaces `shared.ErrUnavailable`). The lease is released first, then the supervisor escalates to terminal so a standby takes over and the orchestrator restarts this pod with a fresh session. |
| **SessionHealthDetail.ConnectAfterLease** | Read-side health flag (`ports.SessionHealthDetail`) marking a source session that defers `Start` until this instance wins the lease (deferred-connect standby). |
| **Stale-while-error** | Resolver policy (`runtime.CredentialResolver`): on a retryable (transient) repository error it serves the last-known-good but expired cached `CredentialSet` instead of failing the rebuild, and does not extend its TTL so the next resolve re-probes the backend. Permanent errors (`NOT_FOUND`, `INVALID_PAYLOAD`, `NOT_AUTHORIZED`) always propagate so a revocation is never masked. Emits `CredentialStaleServed`. |
| **Reactive re-resolve** | Out-of-band credential re-resolve triggered when a live transport reports `NOT_AUTHORIZED` (`CredentialRefresher.NotifyAuthFailure` → `PollBasedWrapper.Refresh`), bypassing the poll timer. Rate-limited per URI to one honoured fetch per `DefaultReactiveReResolveInterval` (5s) so a reconnect storm cannot hammer the backend. |
| **Build-window rotation** | A credential rotation that lands between a session's build-time resolve and its first watch poll. The `PollBasedWrapper` seed compares the cached build value against a fresh uncached read and surfaces the difference as a rotation on start, even when `EmitOnStart` is false. |

## Clock (`domain/clock`)

| Term | Meaning |
|---|---|
| **Clock** | Time abstraction with `Now`, `Since`, `NewTimer`, `NewTicker`, `After`. **No `Sleep`** — every wait must be cancellable via `ctx.Done()`. |
| **Timer / Ticker** | Domain-owned interfaces wrapping `time.Timer` / `time.Ticker`. Faked in tests via `domain/clock/clocktest`. |
| **System clock** | The default `Clock` backed by the stdlib `time` package. |

## Events (`domain/events`)

Layer-1 cross-context **facts** package. The values here are emitted by application/runtime services — **not** raised by aggregates — and published best-effort through `ports.EventPublisher`. See [DDD.md §1](DDD.md#1-bounded-contexts-at-a-glance) and `domain/events/doc.go`.

| Term | Meaning |
|---|---|
| **Event** | An immutable past-tense fact about a bounded-context transition. Carries a stable `EventID`, a namespaced `EventType`, the wall time the fact occurred, the concerned `AggregateID`, and a semver `SchemaVersion`; payload fields are primitives only, so consumers deserialise without importing a producer context. |
| **EventType** | Canonical `<bounded-context>.<aggregate>.<verb-past>` string (e.g. `persistence.outbox.claimed`). Returned by `Event.EventType()`; compare against the exported `SchemaXxx` constants, never a call-site literal. |
| **Emitted fact (not aggregate-raised)** | Aggregates (`OutboxRecord`, the lease, `DLQEntry`, the blueprint) do **not** record or return events on their transitions. The driving application/runtime service constructs the event via a public constructor (e.g. `NewOutboxRecordClaimed`) once the transition has succeeded, then publishes it. |
| **EventPublisher** | The Layer-2 egress port (`ports.EventPublisher`) carrying events to a sink (audit log, message bus, durable outbox). Non-blocking, best-effort: may drop under backpressure and MUST count drops; `NoopEventPublisher` is the null sink. |

## Blueprint / Configuration (`ports/blueprint*.go` + `config/`)

Layer-2 *supporting subdomain*: the parsed-but-not-yet-built shape of a bridge. The `ports/` types are schema-tagged DTOs (yaml/json struct tags, but no yaml/json runtime dependency); the YAML/JSON parser, validator, merger and on-disk store live in `config/`. See [DDD.md §3.7](DDD.md#37-blueprint--configuration--supporting-layer-2).

| Term | Meaning |
|---|---|
| **BridgeConfig** | Aggregate root of the blueprint. Carries `Version` (optimistic-concurrency counter), `Bridge` settings, optional `ConfigWatch`, `Stores`, `Sessions`, `Receivers`, `Senders`, `Bindings`, `Routes`, optional `HTTP`. Consumed whole by `bridge.Builder`. |
| **BridgeSettings** | Bridge-level operational settings: `ID`, `InstanceID`, `DeploymentMode`, shutdown / drain timeouts, `LogLevel`, optional `Cluster`. |
| **SessionDef** | Declarative description of one transport session: `ID`, `Transport`, `Mode` (`ephemeral` / `persistent` / `exclusive`), credential URI, typed plugin `Config`. |
| **BindingDef** | Declarative description of one destination binding: `ID`, `Transport`, `SessionID`, `SenderID`, `Address` (transport destination), typed plugin `Config`, static headers. |
| **RouteDef** | Declarative description of one route: `ID`, `ReceiverID`, `DeliveryMode`, `DispatchMode`, list of `Bindings`, `RoutePolicy`. |
| **BlueprintValidationError** | Structured outcome of blueprint validation. Splits hard `Errors` (block startup or commit) from advisory `Warnings` (surface in admin UI but allow). Returned by the validator and inspected directly by `httpapi`. |
| **ConfigStore** | Port (`ports.ConfigStore`) consumed by the admin HTTP layer for `Load` / `Save` / `Validate` / `Merge` over a `*BridgeConfig`. Decouples `httpapi` from the parser package; composition root supplies the implementation (file-backed, DynamoDB, Vault, …). |
| **Blueprint** | Synonym for an in-memory `*BridgeConfig`: a parsed-but-not-yet-built configuration shape. The blueprint validator runs invariants before `bridge.Builder` is invoked. |
| **RouteSessionDef.AcquirePollInterval** | Blueprint session field (`acquire_poll_interval`, e.g. `"5s"`): how often a standby retries acquiring the lease while another instance owns it. Empty derives `min(renew_interval, lease_ttl/4, 5s)` (floor 1 ms), decoupled from renew so a standby detects a freed lease faster than the owner renews. |
| **RouteSessionDef.RenewCallTimeout** | Blueprint session field (`renew_call_timeout`, e.g. `"3s"`): bounds a single lease-renew store call. Part of the failover-safety invariant — effective renew spacing is `renew_interval + jitter/2 + renew_call_timeout`, so the worst-case loss-detection span folds it in and must stay below `lease_ttl`. Empty derives `min(renew_interval/2, 5s)` (floor 1 s). |
| **ErrApplyInFlight** | `ports.ErrApplyInFlight`: the terminal "committed, NOT confirmed applied, DO NOT roll back" signal of an in-band config apply. Lives in `ports/` so the `httpapi` config-transaction layer can `errors.Is` it across the module boundary. The in-band applier reports exactly three outcomes: `nil` (swap succeeded, runtime on the new config), `ErrApplyInFlight` (config accepted but running state not confirmed — the swap is still in-flight past the apply deadline, or the bridge is paused/shutting down and recorded it for a later resume; the durable config is retained, no rollback), or any other error (definitive failure — the runtime is on the OLD config, so rollback is safe). Surfaced by `httpapi` as `committed_applying` (202). |
| **Manager.AppliedVersion** | `config.Manager.AppliedVersion() (version int, ok bool)`: the `BridgeConfig.Version` this instance last merged, validated, and emitted downstream, and whether any config has been applied yet. A per-instance convergence signal — observation only; GoBridge does not coordinate config versions across the cluster (no version barrier, no cluster rollback), so nodes can diverge. `recordAppliedVersion` logs every change so operators can alert on skew externally. |

## Cross-cutting (Layer 2)

| Term | Meaning |
|---|---|
| **Port** | An interface defined in `ports/` that an adapter implements. The contract crossing the hexagon. |
| **Adapter** | An implementation of a port living in `adapters/<vendor>/<category>/<tech>`. |
| **Driving adapter** | Adapter that calls into the application core (e.g. HTTP API, config loader). |
| **Driven adapter** | Adapter the core calls out to (e.g. transport, store, credential resolver). |
| **Composition root** | `cmd/` — the only place that wires adapters into the runtime. |
| **Bridge** | The composition factory in `bridge/` that turns a parsed `BridgeConfig` into a running `Runtime`. |
| **Runtime** | The use-case engine in `runtime/` that executes routes, drains outboxes, manages leases. |
| **Plugin config** | Transport- or processor-specific typed configuration carried as `any` through the domain and type-asserted at the adapter boundary. |
| **SpanIdentity** | Optional `ports.Span` capability (`ports/tracer.go`) exposing the active span's W3C identity (`TraceID()`/`SpanID()`) so the runtime stamps `trace_id`/`span_id` log fields from the ACTIVE span instead of the upstream `traceparent`. Both return `""` for no-op spans and unsampled traces; the correlation ID remains the always-present join key. |
| **WithStopQuiesce** | Runtime `Option` (`runtime.WithStopQuiesce(budget)`) enabling a bounded pre-cancel drain in `Stop`: the runtime waits up to `budget` for every route to reach `InFlight==0` before cancelling the context, so in-flight deliveries settle instead of aborting into redeliveries. Off by default (abort-style `Stop`). |
| **SwapEvent.Deferred** | `bridge.SwapEvent.Deferred`: true when the Supervisor deliberately did NOT apply `NewConfig` and instead recorded it as the desired state to resume later (currently only when an admin `StopBridge` has paused the bridge). Carries `Error == nil` — not a failure; the config is committed and takes effect on the next `StartBridge`. An in-band applier MUST treat a deferred event as committed-not-applied (no rollback), not as a successful swap. |
| **StorageIdentity** | Optional store `PluginConfig` capability (`StorageIdentity() string`, the `storageIdentifiedConfig` interface) reporting a stable descriptor of a store's DURABLE backing location and nothing tunable: the SQLite file path (`SQLiteConfig`) or the DynamoDB table name (`DynamoDBConfig`). The supervisor's `storeIdentity` compares only this descriptor across a hot reload, so a tuning-only change (e.g. `stale_claim_duration`) is not mistaken for a backing-store repoint — which would strand the old store's durable backlog and is rejected. The descriptor must not contain secrets. |

## Tenancy (`processors/tenant` + `ports/tenant.go`)

| Term | Meaning |
|---|---|
| **TenantUsageReader** | Optional read-back extension of `ports.TenantUsageTracker` (`ports/tenant.go`): `Usage(ctx, tenantID) (TenantUsage, error)`, returning a `TenantUsage` snapshot `{Messages, InFlight}`. The tenant processor type-asserts for it; present alongside a non-zero `TenantInfo.MaxInFlight`, it drives per-tenant in-flight quota enforcement. Kept separate from the increment-only tracker so existing trackers keep compiling. |
| **TenantInfo.MaxInFlight** | Per-tenant ceiling on concurrent in-flight deliveries (`0` = unlimited). Supplied on `TenantInfo` by the embedder's `TenantValidator`, never via YAML. Enforced by the tenant processor only when the usage tracker also implements `TenantUsageReader`; an over-ceiling delivery is rejected transiently with `TENANT_QUOTA_EXCEEDED` (`shared.ErrTenantQuotaExceeded`), so the route retry policy applies backpressure rather than dropping. Distinct from routing's `MaxInFlight` (per-route concurrency cap). |

## Admin HTTP API (`httpapi`)

Driving-adapter names for the admin/monitor HTTP servers. See [docs/http-api.md](docs/http-api.md).

| Term | Meaning |
|---|---|
| **Named admin key** | An admin API key registered under a name in `httpapi.Config.AdminAPIKeys` (or rotated via `AdminAPIKeysProvider`). Possession of the key is the identity: on a successful match the key's name becomes the audit `Actor` — a stable, non-spoofable principal — while the network address (leftmost `X-Forwarded-For` else `RemoteAddr`) demotes to `Detail["client_addr"]`, which stays display-only and spoofable unless a trusted proxy normalises XFF. The legacy single `AdminAPIKey` folds in under the name `admin`; an explicit `admin` entry in the map overrides it. |
| **`committed_applying`** | httpapi config-transaction commit outcome (HTTP 202 Accepted) returned when the durable write succeeded and the runtime ACCEPTED the config but the apply is still in-flight and unconfirmed (the commit returned `ports.ErrApplyInFlight`). A distinct non-5xx status so an operator or automation does not read a 500 as "my change failed" and fire a compensating revert against a runtime that is already converging. Emitted both as the audit outcome and as the JSON `status` field alongside the committed `version`. |

## Transport adapters (`adapters/*/transport`)

Adapter-owned names that surface in config keys, headers, or metrics.

| Term | Meaning |
|---|---|
| **`mqtt.topic`** | Envelope-headers key (`paho.HeaderMQTTTopic`) exposing the MQTT publish topic the broker delivered on. Distinct from the logical subject carried in the `gobridge.subject` user property (`paho.HeaderGobridgeSubject`). |
| **`mqtt.retained`** | Envelope-headers key (`paho.HeaderMQTTRetained` = `"mqtt.retained"`) recording whether the broker delivered the publish with the retained flag set. Bridge-populated at ingress; an inbound user property literally named `mqtt.retained` is dropped so a peer cannot spoof retained state. |
| **`mqtt.qos`** | Envelope-headers key (`paho.HeaderMQTTQoS` = `"mqtt.qos"`) recording the delivered MQTT QoS level. Bridge-populated at ingress; an inbound user property named `mqtt.qos` is dropped. |
| **MQTT ingress identity** | `Envelope.ID` for an inbound MQTT publish. Precedence is a valid `mqtt.message-id`, then valid MQTT correlation data; otherwise the router stamps one UUIDv4 on its owned Paho publish before buffering or fan-out. Packet ID, topic, payload, QoS, and DUP never define fallback identity. A no-ID broker redelivery may therefore receive a new ID and duplicate downstream because MQTT cannot prove publish identity across packet-ID reuse; this at-least-once duplicate is safer than silently collapsing distinct equal-valued events. |
| **DefaultPersistentSessionExpiry** | MQTT: the `session_expiry_interval` (86400 s / 24 h) substituted at Start when a Persistent/Exclusive session leaves the option at `0` — a literal zero would give no offline retention. |
| **Unmatched grace** | MQTT config key `unmatched_grace` (default `DefaultUnmatchedGrace`, 30 s, restarted per (re)connect): window during which a publish matching no registered receiver filter is buffered un-acked. Past the window it is acked, dropped, and its topic unsubscribed. |
| **WillOptions** | MQTT Last Will and Testament block (`session.will.{topic,payload,qos,retain}`), published by the broker on ungraceful disconnect only. Topic required, no `+`/`#` wildcards, QoS 0–2. |
| **MQTTSessionTakeover** | Metric counting server disconnects with reason 0x8E/0x8F — another client connected with the same ClientID. A rising count signals two instances sharing a `client_id`. |
| **MQTTRouterBuffered** | Metric counting publishes held in the MQTT router's bounded pending buffer because they arrived before a matching handler registered. |
| **MQTTRouterUnmatchedDropped** | Metric counting publishes acked-and-dropped after the unmatched grace elapsed — the signature of an orphan broker-side subscription. |
| **MQTT settlement recovery** | Persistent/Exclusive MQTT response to a successful `Delivery.Retry` for QoS 1/2: leave the packet protocol-unsettled, synchronously degrade readiness, stop new ingress, drain accepted settlements for at most 5 s, then rebuild the connection with the same `client_id`, non-zero session expiry, and `clean_start=false`. Retry publishes only queued/degraded request state; active-attempt and target-epoch state become visible atomically after the worker acquires serialization, so an ordinary reconcile that wins first cannot complete or abort queued recovery. Ordinary reconcile, credential/managed reload, orphan cleanup, and recovery share one context-aware session serialization gate. Public entries acquire it with their own context; private reload/reconcile-under-gate helpers never reacquire it, so no nested serialization wait or ABBA order exists. One hard deadline, reused from the conservative post-acquire activation timing, covers gate wait, drain, disconnect, reconnect, and replacement reconcile. Concurrent requests coalesce; completion state and the 30 s minimum interval are published atomically before readiness can recover. Success requires CONNACK Session Present evidence stamped for the exact target connection epoch plus reconciliation of that same epoch; stale or absent evidence fails below Full readiness. Ephemeral Retry remains `shared.ErrNotSupported`. |
| **MQTT current-epoch unsettled packet** | A received QoS 1/2 PUBLISH awaiting successful protocol Ack on the current connection epoch. Tracking starts at receipt and ends at Ack or epoch change. Exposed as `SessionHealth.UnsettledCount`, `OldestUnsettledAge`, and `ReceiveWindowUtilization`, and by the bounded `MQTTUnsettled`, `MQTTOldestUnsettledAge`, and `MQTTReceiveWindowUtilization` metrics tagged only with `session_id`. |
| **MQTTSessionRecoveryRecycle** | Counter metric and `SessionHealth.RecoveryRecycleCount` for completed settlement-recovery recycle attempts. Metric tag cardinality is bounded to `session_id`; producer/message identity is never a dimension. |
| **`use_sessions`** | Service Bus receiver flag: consume a session-enabled entity by accepting the next available session and rotating between sessions (rotate on idle, quiet backoff when none available). Mutually exclusive with `session_id` (pins one session) and `sub_queue`. |
| **`max_lock_renewal_duration`** | Service Bus receiver key (default 5 m): caps total per-delivery lock auto-renewal wall time; on breach the delivery is cancelled and `ASBLockRenewalCapExceeded` increments. |
| **Forward token** | HTTP: shared secret carried in `X-Bridge-Forward-Token` authenticating a peer's `X-Bridge-Forwarded` loop marker (`Factory.WithForwardToken` / `ForwarderConfig.ForwardToken`). Distinct from `api_key` by mandate: it authorises trusting the forwarding marker, not message submission. |
| **Ingress idempotency window** | HTTP: bounded node-local LRU of `Idempotency-Key`/`X-Dedup-Id` values of successfully processed requests (config key `dedup_window`, default 4096). A request presenting a remembered key is acknowledged without re-emitting the delivery. |
| **Redirect endpoint** | HTTP config key `redirect_endpoint`: names the `PeerInfo.Endpoints` key used for opt-in SSE 307 redirects to the route owner. Empty (default) refuses with 503 so the internal peer endpoint never leaks to an external client. |
| **CapExclusiveIdentity** | Transport capability `exclusive_identity` (`ports.CapExclusiveIdentity`) marking a lease-based single-holder session whose reconfig is identity-sensitive (the supervisor serializes the swap). Declared by MQTT (paho) always and by amqp091 once it has built an exclusive consumer (latched); see also `ConfigRequiresExclusiveIdentity`. Exclusive-identity transports are subject to the Single-use exclusive session invariant. |
| **CapSharedConsumer** | Transport capability `shared_consumer` (`ports.CapSharedConsumer`) marking a broadcast/scale-out source where the broker load-balances one logical subscription across a consumer group. Declared by MQTT (paho) for `$share/<group>/<filter>` shared subscriptions; deliberately omitted by SQS. |
| **ConfigRequiresExclusiveIdentity** | amqp091 `Factory` hook (`ConfigRequiresExclusiveIdentity(cfg)`) reporting whether a receiver plugin config declares an exclusive consumer, so the supervisor selects the serialized swap mode on the first reconfig that introduces exclusivity — before any receiver (and the internal `exclusive_identity` latch) exists. |
| **MetricAMQP10DelayedRetryDeferred** | AMQP 1.0 metric `AMQP10DelayedRetryDeferred` counting delayed (backoff) retries whose redelivery timing was deferred to the broker (a `ModifyMessage` carrying an `x-opt-delivery-time` annotation). It measures broker-delegated retry scheduling, not a failure. Renamed from `AMQP10DelayedRetryUnhonored`, which wrongly implied the broker ignored the request on every delayed retry. |
| **ErrTemporaryCredentialsUnsupported** | SQS adapter error (`sqs.ErrTemporaryCredentialsUnsupported`) rejecting temporary/STS material — an `ASIA`-prefixed access key that needs a session token the static password credential cannot carry. Wrapped as `shared.ErrNotAuthorized` (code `NOT_AUTHORIZED`, permanent) so callers do not retry it. |
| **`allow_insecure_plain`** | AMQP 1.0 session config key (`SessionOptions.AllowInsecurePlain`). Opt-in that permits SASL PLAIN (username/password) over a non-TLS `amqp://` (or schemeless) address, where the credentials travel on the wire in cleartext frames. Rejected at config validation by default (`c7-plain-plaintext`, secure-by-default) — including credentials embedded in the address userinfo; set `true` only on a trusted network or for local development. No effect on a TLS scheme (`amqps://` / `amqp+ssl://`), where PLAIN is already protected. |
| **Dedicated-session contract (durable AMQP 1.0)** | Requirement that a durable AMQP 1.0 receiver (`durability_mode > 0`, a durable multicast subscription) is placed on its OWN session (its own `session_id`), not multiplexed with other receivers/senders. Closing a durable receiver forces a full connection teardown — the pinned go-amqp can only emit a closing detach, which Artemis reads as UNSUBSCRIBE and destroys the durable terminus — so the close transiently blips every sibling link on the same session (in-flight sender publishes relatch, non-durable receivers redeliver). Isolating durable receivers on a dedicated session confines that blast radius (`c7-durable-close`). |
| **Fallback message-ID scope (`q:` / `s:` / `t:`)** | Service Bus entity-namespaced prefix (`entityScopeFor`) that scopes the sequence-number-derived fallback envelope ID (`stableFallbackID`, format `asb-seq:<entity>:<sequence>`) when a received message carries an empty broker MessageID. `q:<queue>`, `s:<topic>:<subscription>`, or `t:<topic>`. The broker SequenceNumber is unique only WITHIN an entity, so an un-namespaced `asb-seq:<n>` could collide across queues/subscriptions and let a cross-entity dedup store drop a distinct message (data loss). The mapping is provably injective — the kinds differ in their first two chars and `:` is disallowed in every ASB entity name — so no two entities share a scope. |
| **`allow_unroutable_drop`** | amqp091 sender config key (`SenderConfig.AllowUnroutableDrop`). Explicit opt-in that lets a managed sender publish with `mandatory=false`, where the broker CONFIRMS an unroutable publish and then silently DISCARDS it — the bridge acks the source and the message is lost with no telemetry (wrong routing key / missing binding after a deploy). The managed factory refuses to build a sender unless the publish is mandatory OR this flag admits the silent-drop behaviour deliberately. It records that the operator accepts the loss; it never changes the publish. |
| **`allow_at_most_once`** | Service Bus receiver config key (`ReceiverConfig.AllowAtMostOnce`). Explicit opt-in required to run `receive_mode` `ReceiveAndDelete`, which is at-most-once: the broker deletes the message at receive time, so a crash (or malformed-envelope drop) after receive is unrecoverable loss, `Ack` is a no-op, and `Retry` is unsupported. `validate()` rejects `ReceiveAndDelete` unless set and `NewReceiver` emits a loud startup warning. Ignored for `PeekLock`. |
| **MetricMQTTQoSDowngraded** | MQTT metric `MQTTQoSDowngraded` (`paho.MetricMQTTQoSDowngraded`) counting subscriptions the broker GRANTED at a lower QoS than requested (e.g. requested QoS 2, SUBACK granted QoS 0). The adapter emits a loud warning, returns `ErrQoSNotSupported` with topic/requested/granted context, and does not mark the filter active, so readiness remains non-Full. Tagged `session_id` (the `client_id`). Any non-zero value warrants investigating a broker QoS-cap policy. |

## Store adapters (`adapters/*/store`)

Adapter-owned names for the lease / outbox / DLQ / managed-subscription persistence stores, surfacing in config keys, functional options, and DynamoDB schema.

| Term | Meaning |
|---|---|
| **`acknowledge_single_replica`** | memorylease store option/config key (`memorylease.WithAcknowledgeSingleReplica(true)`). The operator's explicit acknowledgement that this process is the ONLY replica that will ever use the in-memory lease store. An in-memory lease map cannot coordinate ownership across processes, so the store FAILS CLOSED — every operation returns `shared.ErrInvalidConfig` until the option is passed with `true` — and passing `true` emits one loud split-brain warning (`c10-memlease-split`). Required before the store may back exclusive sessions. |
| **WithTTLPreflightAdvisory** | dynamodblease store `Option` (`dynamodblease.WithTTLPreflightAdvisory`), mirrored on the AWS store factory (`awsstore.WithTTLPreflightAdvisory`, a `FactoryOption`). Explicit dev/emulator opt-out that downgrades the build-time lease-table TTL preflight from fail-closed to advisory (loud WARN, build continues). The default fail-closed check requires `dynamodb:DescribeTimeToLive` and blocks boot on an enabled/enabling DynamoDB TTL over the fencing table, where TTL would delete the fencing counter of record. `WithSchemaPreflightAdvisory` does NOT relax this — only this TTL-specific option does. |
| **WithSchemaPreflightAdvisory** | DynamoDB store `FactoryOption` (`awsstore.WithSchemaPreflightAdvisory`). Explicit dev/emulator opt-out that downgrades the build-time DynamoDB schema preflight from fail-closed (the production default) to advisory: when `DescribeTable` cannot VERIFY the target table — a control-plane throttle, a least-privilege role lacking `dynamodb:DescribeTable` (AccessDenied), or an emulator without `DescribeTable` — the factory logs a loud WARN and builds the store anyway. A confirmed schema mismatch (`shared.ErrInvalidConfig`) stays FATAL regardless of the flag. |
| **ClaimIndex / `claim_sort`** | dynamodboutbox claim GSI (`claimIndexName = "ClaimIndex"`) and its range key (`attrClaimSort = "claim_sort"`). `claim_sort` encodes `(created_at millis, seq)` as a zero-padded, lexicographically-sortable string, stamped by Persist and REMOVED at a terminal transition (Complete/Expire), so the sparse `ClaimIndex` (`hash=PK, range=claim_sort`, `ScanIndexForward=true`) returns a partition's claimable records oldest-first and Claim stops after `limit` instead of scanning the whole partition (`c13-claim-quadratic`). Optional — an absent index degrades Claim to a whole-partition scan — but a present `ClaimIndex` MUST be `Projection: ALL` or preflight rejects it at startup, because the claim query filters on a non-key attribute. |

## Deployment / seeding (`deployment/aws-filebased-config`)

Deploy-time constructs and worker/control seeding for the AWS file-based topology. See [docs/aws-deployment/overview.md](docs/aws-deployment/overview.md).

| Term | Meaning |
|---|---|
| **AdoptValid** | Worker seeder mode (the worker default). Requires the EFS `bridge.yaml` to exist and parse, then adopts it as-is — from either the CDK seed or an Admin-API config-txn commit — never failing on hash drift versus the synth-time asset. Absent or unparseable config still fails. |
| **AbortDeploy** | Strict opt-in worker seeder mode (`WorkerSeederMode`). The task aborts unless the EFS `bridge.yaml` exists and its canonical hash matches the deployed asset exactly, forcing lock-step config across the fleet. |
| **SeedOnce** | Control-node default seeder mode. Writes the asset only when the target is absent; otherwise keeps the existing config (warns on hash drift). |
