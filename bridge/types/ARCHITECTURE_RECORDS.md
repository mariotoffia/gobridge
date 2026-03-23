# Architecture Records

This document records the main architecture decisions behind the proposed GoBridge redesign.

Status values:

- `Proposed`: recommended for implementation, not yet adopted in code
- `Superseded`: kept for historical traceability

## Index

| ID | Title | Status |
|----|-------|--------|
| AR-001 | Simplify the core around envelope, delivery, receiver, sender, and session | Proposed |
| AR-002 | Use absolute expiry instead of bridge-local TTL | Proposed |
| AR-003 | Make session ownership explicit with leases for single-active modes | Proposed |
| AR-004 | Treat MQTT subscriptions as session state and reconcile desired state | Proposed |
| AR-005 | Use shared subscriptions when the broker supports them, otherwise single-active ownership | Proposed |
| AR-006 | Introduce a durable outbox for reliable egress | Proposed |
| AR-007 | Keep middleware pure and move delivery control into the runtime | Proposed |
| AR-008 | Forbid local drops in reliable modes | Proposed |
| AR-009 | Target MQTT 5 as the primary design point and degrade to MQTT 3.1.1 subset | Proposed |
| AR-010 | Split session config from receiver and sender specs | Proposed |
| AR-011 | Resolve egress through destination bindings and dispatch plans | Proposed |
| AR-012 | Keep runtime ports in the core module and place concrete adapters in technology modules | Proposed |
| AR-013 | Define a standard header set with reserved prefix for bridge-internal headers | Proposed |
| AR-014 | Require fencing tokens on lease operations to prevent split-brain publishing | Proposed |
| AR-015 | Decompose route runner into composable stages for testability | Proposed |
| AR-016 | Require poison message protection with replay count limits on outbox entries | Proposed |
| AR-017 | Per-route delivery mode selection (DirectHold vs SharedOutbox) | Proposed |
| AR-018 | DynamoDB-first shared stores with portable interfaces | Proposed |

## AR-001: Simplify the core around envelope, delivery, receiver, sender, and session

**Status:** Proposed

### Context

The current core models ingress, egress, and broker state multiple times through:

- `Source`
- `Target`
- `Connection`
- `Publisher`
- `SubscriberSource`
- `Subscriber`

That makes transport implementations more complex than necessary.

### Decision

The proposed core standardizes on:

- `Envelope`
- `Delivery`
- `Receiver`
- `Sender`
- `Session`

### Consequences

- The mental model becomes smaller.
- MQTT-specific behavior has a natural home in `Session`.
- Legacy publish and subscribe abstractions should be deprecated from the core.

## AR-002: Use absolute expiry instead of bridge-local TTL

**Status:** Proposed

### Context

The current `Message.TTL` is used both as:

- retry budget
- bridge-local expiry
- MQTT v5 message expiry source

This creates ambiguity and can lead to incorrect expiry propagation.

### Decision

Use `Envelope.ExpiresAt` as the canonical expiry in the bridge core.

Transport-native expiry values are derived from remaining lifetime when a send happens.

### Consequences

- Retry policy becomes simpler.
- MQTT v5 message expiry becomes correct by construction.
- MQTT 3.1.1 and other transports without native expiry can still be supported.

## AR-003: Make session ownership explicit with leases for single-active modes

**Status:** Proposed

### Context

Some routes must use a single named remote identity such as an MQTT `ClientID`.

In clustered deployments this is fundamentally an ownership problem.

### Decision

Introduce lease-based ownership for session identities that must be single-owner.

Only the lease holder may operate the session.

### Consequences

- Failover becomes an ownership transfer, not ad hoc object migration.
- Named MQTT clients can be handled safely in cluster mode.
- Cluster coordination stays generic.

## AR-004: Treat MQTT subscriptions as session state and reconcile desired state

**Status:** Proposed

### Context

MQTT subscriptions live at session scope, not at arbitrary bridge object scope.

The current design uses per-source subscribe and unsubscribe operations on shared connections, which is hard to reason about during reconnect and failover.

### Decision

Represent desired MQTT state as a `SessionPlan` and reconcile it on connect and reconnect.

### Consequences

- Reconnect behavior becomes deterministic.
- Shared session subscription state can be reference-counted or reconciled centrally.
- Lifecycle transactions become less important than desired-state reconciliation.

## AR-005: Use shared subscriptions when the broker supports them, otherwise single-active ownership

**Status:** Proposed

### Context

The bridge needs to support active/active and failover patterns for MQTT subscriptions.

MQTT 5 shared subscriptions are broker-native; MQTT 3.1.1 standard does not define them.

### Decision

- Use MQTT shared subscriptions for active/active consumer groups when supported.
- Fall back to single-active lease ownership when they are not supported.

### Consequences

- The bridge does not need to emulate shared consumption in the core.
- Horizontal scale stays aligned with broker behavior.
- MQTT 3.1.1 deployments still have a clear failover model.

## AR-006: Introduce a durable outbox for reliable egress

**Status:** Proposed

### Context

Reliable source transports such as SQS can deliver a message durably, but a stateful or intermittent target such as MQTT may be unavailable at the moment of forwarding.

Without durable bridge-local ownership, ack timing is unsafe.

### Decision

Add a durable outbox to the route runtime.

Use it whenever source ack must occur before destination acceptance can be guaranteed immediately.

### Consequences

- SQS -> MQTT routes can survive process restart and target downtime.
- Retry logic becomes operationally clearer.
- Outbox storage becomes a required runtime dependency for some routes.

## AR-007: Keep middleware pure and move delivery control into the runtime

**Status:** Proposed

### Context

The current design mixes message processing concerns with delivery concerns.

### Decision

Limit middleware to envelope processing:

- transform
- filter
- validate
- enrich

Keep retry, backpressure, expiry, outbox, and DLQ in the route runtime.

### Consequences

- Middleware is easier to test.
- Delivery reasoning is centralized.
- The retry story becomes less fragmented.

## AR-008: Forbid local drops in reliable modes

**Status:** Proposed

### Context

If a transport delivers a message at QoS 1 or 2 and the bridge drops it due to internal queue pressure, the bridge has already broken the intended reliability contract.

### Decision

Reliable routes must block, slow down, or negotiate lower intake. They must not drop locally due to full channels.

### Consequences

- Backpressure must be designed deliberately.
- MQTT 5 `Receive Maximum` should be aligned with route policy where possible.
- "Channel full" becomes a bug in reliable routes, not an acceptable runtime outcome.

## AR-009: Target MQTT 5 as the primary design point and degrade to MQTT 3.1.1 subset

**Status:** Proposed

### Context

MQTT 5 offers the protocol features needed for a clean architecture:

- session expiry
- message expiry
- shared subscriptions
- no local
- retain handling
- receive maximum
- explicit disconnect reasons

MQTT 3.1.1 lacks some of these features.

### Decision

Design the architecture around MQTT 5 semantics and degrade gracefully when an MQTT 3.1.1 broker is used.

### Consequences

- The design stays clean for modern brokers.
- The fallback behavior for MQTT 3.1.1 is explicit rather than accidental.
- Some route policies may only be fully supported on MQTT 5.

## AR-010: Split session config from receiver and sender specs

**Status:** Proposed

### Context

The current transport configs often embed full connection settings into source and target configs, even when they share one underlying session.

### Decision

Separate:

- `SessionSpec`
- `ReceiverSpec`
- `SenderSpec`

Endpoint specs reference the session by ID when shared session mode is used.

### Consequences

- Configuration duplication is reduced.
- Ownership is clearer.
- Session failover and endpoint routing can evolve independently.

## AR-011: Resolve egress through destination bindings and dispatch plans

**Status:** Proposed

### Context

Some routes must choose the concrete egress target dynamically.

Examples:

- one SQS queue publishes to different MQTT clients
- one stream fans out to MQTT and Azure Service Bus
- one logical message subject must render to transport-specific addresses

Without an explicit routing abstraction, the runtime cannot reliably determine:

- which sender should be used
- which session lease is required
- which outbox partition should own replay

### Decision

Introduce:

- `DestinationBinding` as configured egress target metadata
- `DispatchPlan` as the resolved concrete destination for one envelope
- `DestinationResolver` as the route-level component that maps an envelope to one or more dispatch plans

### Consequences

- "send through client X to topic Y" becomes explicit.
- Outbox partitioning and lease ownership become deterministic.
- Fan-out across multiple target transports stays generic.

## AR-012: Keep runtime ports in the core module and place concrete adapters in technology modules

**Status:** Proposed

### Context

GoBridge already uses a multi-module workspace so the core can stay lightweight while provider SDKs live in adapter modules.

The redesign adds new infrastructure concerns such as:

- `LeaseStore`
- `OutboxStore`

Those also need concrete implementations without polluting the core module with cloud SDK dependencies.

### Decision

Keep ports and runtime contracts in the core module.

Place concrete implementations in mirrored technology modules such as:

- `technologies/aws/transport/sqs`
- `technologies/aws/store/dynamodblease`
- `technologies/aws/store/dynamodboutbox`
- `technologies/native/store/sqliteoutbox`

### Consequences

- multi-module boundaries stay consistent
- cloud dependencies remain outside the core
- lease and outbox implementations have a clear home
- provider-specific adapter discovery becomes easier

## AR-013: Define a standard header set with reserved prefix for bridge-internal headers

**Status:** Proposed

### Context

`Envelope.Headers` is `map[string]any` with no reserved keys, no validation, and no correlation or tracing propagation. The `DestinationResolver` uses type assertions on header values for routing decisions, which creates an injection vector if external sources can set arbitrary headers. Without standard headers, correlation IDs, idempotency keys, and trace context are not consistently propagated across transport boundaries.

### Decision

Define a reserved header prefix (`x-bridge.`) and a small set of well-known header constants for correlation, causation, idempotency, content type, source identity, ordering, and W3C trace context. Keep `map[string]any` for extensibility. Transport adapters must strip reserved-prefix headers from external sources at ingress and map between standard headers and transport-native fields.

### Consequences

- Correlation and trace context propagation becomes consistent across transports.
- Header injection from external sources is prevented by ingress stripping.
- Idempotency keys have a defined location and can be enforced by the startup validator.
- Transport adapters must implement bidirectional header mapping.

## AR-014: Require fencing tokens on lease operations to prevent split-brain publishing

**Status:** Proposed

### Context

The original `Lease` interface had `Acquire`, `Renew`, and `Release` methods that returned only errors. Without a fencing token, a stale owner surviving a GC pause or network partition could resume publishing through an exclusive MQTT session after a new owner had already acquired the lease.

### Decision

`Acquire` and `Renew` return a `LeaseToken` with a monotonically increasing `Version`. All `OutboxStore` mutations must accept and validate the fencing token. The sender must verify its token before each outbox drain batch.

### Consequences

- Split-brain publishing is prevented by fencing token validation at the store level.
- The `OutboxStore` interface is coupled to the fencing token, which adds complexity but is necessary for correctness.
- Lease store implementations must provide monotonically increasing versions, which maps naturally to DynamoDB conditional writes.

## AR-015: Decompose route runner into composable stages for testability

**Status:** Proposed

### Context

The route runner is responsible for backpressure, expiry checking (at three points), processor chain execution, outbox persistence, sender dispatch, DLQ routing, retry logic, ack timing, and lease interaction. Centralizing all delivery control is correct, but a single component with 9+ responsibilities risks becoming a god object.

### Decision

Decompose the route runner into composable stages: `ExpiryGuard`, `ProcessorChain`, `OutboxWriter`, `SenderDispatcher`, `AckCoordinator`, and `DLQRouter`. Each stage is independently testable. The route runner composes them into the execution pipeline.

### Consequences

- Each stage can be tested in isolation.
- New stages can be added without modifying the route runner.
- The route execution flow remains centralized but the implementation is distributed across focused components.

## AR-016: Require poison message protection with replay count limits on outbox entries

**Status:** Proposed

### Context

A malformed message in the outbox that causes the sender to crash creates an infinite crash loop across lease transfers. Without a replay count limit, the same message is claimed, causes a crash, reverts to pending, and is claimed again by the next lease holder.

### Decision

Every outbox record carries a `ReplayCount` that is incremented on each `Claim`. When `ReplayCount` exceeds a configurable `MaxReplayAttempts` (default: 5), the record is moved to the DLQ and marked as expired.

### Consequences

- Infinite crash loops from poison messages are prevented.
- The DLQ receives detailed context for diagnosis.
- A small number of messages may be dropped after exceeding the limit, which is acceptable for at-least-once semantics.

## AR-017: Per-route delivery mode selection (DirectHold vs SharedOutbox)

**Status:** Proposed

### Context

Routes bridging from durable sources (SQS) to stateful targets (MQTT) need explicit delivery mode configuration. Without it, the runtime cannot determine whether to use the outbox or rely on source visibility extension. Implicit selection based on runtime capabilities is fragile and hard to reason about during operations.

### Decision

Introduce `DeliveryMode` as an explicit route policy field with two values:

- `DirectHold`: Source retains message visibility while the bridge processes and sends. No outbox persistence. Requires co-located consumer and sender, single-binding dispatch, visibility extension support, and no lease handoff.
- `SharedOutbox`: Bridge takes durable ownership via the outbox before acking the source. Required for cross-instance handoff, fan-out, exclusive session ownership, and long target outages.

The startup validator enforces compatibility rules for each mode and produces clear error messages for invalid configurations.

### Consequences

- Operators make explicit latency vs durability tradeoffs per route.
- Invalid configurations fail at startup, not at runtime.
- The runtime execution path is deterministic based on the configured mode.

## AR-018: DynamoDB-first shared stores with portable interfaces

**Status:** Proposed

### Context

The clustered runtime requires shared `LeaseStore` and `OutboxStore` implementations. Multiple backend options exist (DynamoDB, Postgres, Redis). Attempting to abstract over all of them prematurely adds implementation and testing complexity without delivering value in the phase-1 scope.

### Decision

- DynamoDB is the phase-1 production store for both `LeaseStore` and `OutboxStore`.
- SQLite and in-memory stores are test-only adapters in `technologies/native/store/`.
- Postgres may be added as a production backend later, chosen explicitly rather than through generic SQL abstraction.
- Store interfaces remain in `bridge/types/` and are backend-neutral.

### Consequences

- Phase-1 implementation focuses on one production backend.
- DynamoDB conditional writes map naturally to fencing semantics.
- Store interfaces are already portable; adding Postgres later does not require interface changes.
- Non-AWS deployments can use test stores until a Postgres adapter is available.

## Superseded Ideas

These ideas are intentionally not carried forward into the proposed design:

- Modeling MQTT source lifecycle primarily as add and remove operations on endpoint objects.
- Treating route retry behavior as two separate conceptual systems in normal user-facing documentation.
- Carrying publish and subscribe callback abstractions in the core type system alongside receiver and sender abstractions.

## Notes For Implementation

These records describe the target architecture.

Implementation should proceed in stages:

1. introduce new docs and align terminology
2. split session and endpoint configs
3. add route policy and outbox primitives
4. refactor MQTT around session reconciliation
5. deprecate legacy publish and subscribe core abstractions
