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

## Messaging (`domain/messaging`)

| Term | Meaning |
|---|---|
| **Envelope** | The canonical message moving through the bridge. `ID`, `Subject`, `Payload`, `Headers`, `CreatedAt`, `ExpiresAt`. Aggregate root of the messaging context. |
| **Subject** | Logical event subject of an envelope. Free-form string set by the producer (or by an ingress mapping). The runtime never overwrites it with a transport destination. Distinct from `Address`. |
| **Address** | Transport destination chosen at egress time for one envelope: a publish topic, AMQP routing key, queue URL, etc. Lives on `DestinationBinding.Address`, `DispatchPlan.Address`, `OutboxRecord.Address`, and `ports.OutboundMessage.Address`. Never written into `Envelope.Subject`. |
| **OutboundMessage** | The unit `Sender.Send`/`BatchSender.SendBatch` consume: `{Envelope *domain.Envelope, Address string}`. Carries the logical envelope and the resolved transport destination side-by-side. |
| **`gobridge.subject`** | Cross-transport carrier for `Envelope.Subject` on transports without a native subject field (MQTT user property, AMQP 0-9-1 header). Application-visible, not reserved. |
| **Reserved header** | Header key with prefix `x-bridge.` (case-insensitive). Bridge-internal; must be stripped from external sources at ingress. |
| **Correlation ID** | `x-bridge.correlation-id`. End-to-end ID used to correlate logs/traces across hops. |
| **Causation ID** | `x-bridge.causation-id`. ID of the envelope that caused this one to be produced. |
| **Idempotency key** | `x-bridge.idempotency-key`. Used by adapters and outbox to dedup. |
| **Ordering key** | `x-bridge.ordering-key`. Hint to ordered transports (e.g. SQS FIFO group). |
| **Dedup ID** | `x-bridge.dedup-id`. Transport-level deduplication identifier. |
| **Forwarded-from / Forwarded-hop** | Cluster-forwarding lineage headers. |
| **TraceContext** | W3C Trace Context (`traceparent` + `tracestate`). Parsed/formatted/extracted/injected only via the helpers in `messaging`. |

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
| **LeaseInfo** | Authoritative lease state: `LeaseID`, `Owner`, `Version`, `ExpiresAt`, `Endpoints`. |
| **LeaseToken** | Fencing token `{Owner, Version}` returned with the lease. Carried on every guarded write so stale owners are rejected. |
| **Fencing token / Fencing** | The mechanism that uses `LeaseToken.Version` (monotonic) to reject writes from a previous owner after a lease transfer. |
| **Stale fencing token** | A write attempted with an older `Version` than the current lease. Rejected with `shared.ErrCodeStaleFencingToken`. |
| **Replay** | Re-attempting outbox dispatch of a previously claimed record. Counted by `ReplayCount`, capped by `RoutePolicy.MaxReplayAttempts`. |
| **PeerInfo** | Remote bridge instance discovered via lease ownership history. |

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
| **CredentialSet** | Composite credential container — may carry both a `PasswordCredential` and `TLSMaterial`. Aggregate root. |
| **PasswordCredential** | `Username` + `Password`. Redacts in `String`/`GoString`. |
| **TLSMaterial** | `CertPEM`, `KeyPEM`, `CAPEMs`, `InsecureSkipVerify`. Redacts in `String`/`GoString`. |
| **Push credential / Pull credential** | Two delivery modes for credential rotation: push stores emit on change; pull stores are polled. `CredentialSet.Equal` is the canonical change check. |

## Clock (`domain/clock`)

| Term | Meaning |
|---|---|
| **Clock** | Time abstraction with `Now`, `Since`, `NewTimer`, `NewTicker`, `After`. **No `Sleep`** — every wait must be cancellable via `ctx.Done()`. |
| **Timer / Ticker** | Domain-owned interfaces wrapping `time.Timer` / `time.Ticker`. Faked in tests via `domain/clock/clocktest`. |
| **System clock** | The default `Clock` backed by the stdlib `time` package. |

## Blueprint / Configuration (`ports/blueprint*.go` + `config/`)

Layer-2 *supporting subdomain*: the parsed-but-not-yet-built shape of a bridge. Format-neutral types live in `ports/`; the YAML/JSON parser, validator, merger and on-disk store live in `config/`. See [DDD.md §3.7](DDD.md#37-blueprint--configuration--supporting-layer-2).

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
