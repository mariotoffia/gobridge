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

## Routing (`domain/routing`)

| Term | Meaning |
|---|---|
| **Route** | A logical edge from a source binding through processors to one or more destination bindings, governed by a `RoutePolicy`. |
| **RoutePolicy** | Per-route delivery, retry, backpressure, and timeout configuration. Aggregate root for routing. |
| **BackoffPolicy** | Retry backoff parameters: `InitialInterval`, `MaxInterval`, `Multiplier`. |
| **DeliveryMode** | `direct_hold` (hold source ack until target accepts) or `shared_outbox` (persist then ack source). |
| **DispatchMode** | `single` (one binding per envelope) or `fan_out` (every matching binding). |
| **AckBoundary** | `target_accept` (ack source after target accepts) or `outbox_persist` (ack source after outbox persists). Together with `DeliveryMode` determines the at-least-once / once-and-only contract per route. |
| **ExpiredAction** | What happens to expired envelopes: `drop` or `dlq`. |
| **FailureAction** | What happens on permanent failure: `dlq` or `drop`. |
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
| **SessionPlan** | The desired state of a session: list of `SubscriptionPlan` and `PublisherPlan`. Adapters reconcile actual state toward the plan. |
| **SubscriptionPlan** | Desired subscription: `Topic`, `QoS`, transport-typed `Config`. |
| **PublisherPlan** | Desired publisher: `Topic`, `QoS`, transport-typed `Config`. |
| **Reconcile** | The act of bringing a session's actual subscriptions/publishers in line with its `SessionPlan`. |
| **Credential** | Authentication material. One of: `password`, `tls`. |
| **CredentialSet** | Composite credential container — may carry both a `PasswordCredential` and `TLSMaterial`; accessed via nil-safe accessors `Password()` and `TLS()`; constructed via `NewCredentialSet(password, tls)`. Machine-enforced aggregate root. |
| **PasswordCredential** | `Username` + `Password`. Redacts in `String`/`GoString`. |
| **TLSMaterial** | `CertPEM`, `KeyPEM`, `CAPEMs`, `InsecureSkipVerify`. Redacts in `String`/`GoString`. |
| **Push credential / Pull credential** | Two delivery modes for credential rotation: push stores emit on change; pull stores are polled. `CredentialSet.Equal` is the canonical change check. |

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

## Tenancy (`processors/tenant` + `ports/tenant.go`)

| Term | Meaning |
|---|---|
| **TenantUsageReader** | Optional read-back extension of `ports.TenantUsageTracker` (`ports/tenant.go`): `Usage(ctx, tenantID) (TenantUsage, error)`, returning a `TenantUsage` snapshot `{Messages, InFlight}`. The tenant processor type-asserts for it; present alongside a non-zero `TenantInfo.MaxInFlight`, it drives per-tenant in-flight quota enforcement. Kept separate from the increment-only tracker so existing trackers keep compiling. |
| **TenantInfo.MaxInFlight** | Per-tenant ceiling on concurrent in-flight deliveries (`0` = unlimited). Supplied on `TenantInfo` by the embedder's `TenantValidator`, never via YAML. Enforced by the tenant processor only when the usage tracker also implements `TenantUsageReader`; an over-ceiling delivery is rejected transiently with `TENANT_QUOTA_EXCEEDED` (`shared.ErrTenantQuotaExceeded`), so the route retry policy applies backpressure rather than dropping. Distinct from routing's `MaxInFlight` (per-route concurrency cap). |

## Transport adapters (`adapters/*/transport`)

Adapter-owned names that surface in config keys, headers, or metrics.

| Term | Meaning |
|---|---|
| **`mqtt.topic`** | Envelope-headers key (`paho.HeaderMQTTTopic`) exposing the MQTT publish topic the broker delivered on. Distinct from the logical subject carried in the `gobridge.subject` user property (`paho.HeaderGobridgeSubject`). |
| **DefaultPersistentSessionExpiry** | MQTT: the `session_expiry_interval` (86400 s / 24 h) substituted at Start when a Persistent/Exclusive session leaves the option at `0` — a literal zero would give no offline retention. |
| **Unmatched grace** | MQTT config key `unmatched_grace` (default `DefaultUnmatchedGrace`, 30 s, restarted per (re)connect): window during which a publish matching no registered receiver filter is buffered un-acked. Past the window it is acked, dropped, and its topic unsubscribed. |
| **WillOptions** | MQTT Last Will and Testament block (`session.will.{topic,payload,qos,retain}`), published by the broker on ungraceful disconnect only. Topic required, no `+`/`#` wildcards, QoS 0–2. |
| **MQTTSessionTakeover** | Metric counting server disconnects with reason 0x8E/0x8F — another client connected with the same ClientID. A rising count signals two instances sharing a `client_id`. |
| **MQTTRouterBuffered** | Metric counting publishes held in the MQTT router's bounded pending buffer because they arrived before a matching handler registered. |
| **MQTTRouterUnmatchedDropped** | Metric counting publishes acked-and-dropped after the unmatched grace elapsed — the signature of an orphan broker-side subscription. |
| **`use_sessions`** | Service Bus receiver flag: consume a session-enabled entity by accepting the next available session and rotating between sessions (rotate on idle, quiet backoff when none available). Mutually exclusive with `session_id` (pins one session) and `sub_queue`. |
| **`max_lock_renewal_duration`** | Service Bus receiver key (default 5 m): caps total per-delivery lock auto-renewal wall time; on breach the delivery is cancelled and `ASBLockRenewalCapExceeded` increments. |
| **Forward token** | HTTP: shared secret carried in `X-Bridge-Forward-Token` authenticating a peer's `X-Bridge-Forwarded` loop marker (`Factory.WithForwardToken` / `ForwarderConfig.ForwardToken`). Distinct from `api_key` by mandate: it authorises trusting the forwarding marker, not message submission. |
| **Ingress idempotency window** | HTTP: bounded node-local LRU of `Idempotency-Key`/`X-Dedup-Id` values of successfully processed requests (config key `dedup_window`, default 4096). A request presenting a remembered key is acknowledged without re-emitting the delivery. |
| **Redirect endpoint** | HTTP config key `redirect_endpoint`: names the `PeerInfo.Endpoints` key used for opt-in SSE 307 redirects to the route owner. Empty (default) refuses with 503 so the internal peer endpoint never leaks to an external client. |

## Deployment / seeding (`deployment/aws-filebased-config`)

Deploy-time constructs and worker/control seeding for the AWS file-based topology. See [docs/aws-deployment/overview.md](docs/aws-deployment/overview.md).

| Term | Meaning |
|---|---|
| **AdoptValid** | Worker seeder mode (the worker default). Requires the EFS `bridge.yaml` to exist and parse, then adopts it as-is — from either the CDK seed or an Admin-API config-txn commit — never failing on hash drift versus the synth-time asset. Absent or unparseable config still fails. |
| **AbortDeploy** | Strict opt-in worker seeder mode (`WorkerSeederMode`). The task aborts unless the EFS `bridge.yaml` exists and its canonical hash matches the deployed asset exactly, forcing lock-step config across the fleet. |
| **SeedOnce** | Control-node default seeder mode. Writes the asset only when the target is absent; otherwise keeps the existing config (warns on hash drift). |
