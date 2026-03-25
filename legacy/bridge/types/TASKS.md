# `TASKS.md` Plan: New Architecture Implementation, AWS-First

## Summary

Create `bridge/types/TASKS.md` as the canonical execution plan for the new architecture docs. It should sit alongside the proposal set and become the implementation checklist for a **big-bang cutover** to the new runtime.

The plan should lock these decisions:

- Phase 1 production scope: MQTT 5, AWS SQS, DynamoDB-backed `LeaseStore`, DynamoDB-backed `OutboxStore`
- Support **both** `direct_hold` and `shared_outbox` for `SQS -> MQTT`
- `direct_hold` is allowed only for **co-located single-binding** routes
- `shared_outbox` is required for decoupled ingress/egress ownership, exclusive MQTT session handoff, long outages, or fan-out
- SQL is **not** the phase-1 production store; keep the store interfaces neutral so a later Postgres adapter can be added cleanly
- Add `sqliteoutbox` only as a native/local test adapter in phase 1

`TASKS.md` should also require updates to the architecture docs so they match these delivery-mode and store decisions.

## Architecture Doc Updates

Before code work, update the proposal docs so they become implementation-accurate:

- Add a delivery-mode section to `ARCHITECTURE_NEW-MIDDLEWARE.md`:
  - `direct_hold`
  - `shared_outbox`
  - startup validation rules
  - outbox state machine: `pending`, `claimed`, `completed`, `expired`
- Add a “when outbox is required vs optional” section to `ARCHITECTURE_NEW-CLUSTERING.md`
  - fan-out outbox atomicity requirement with DynamoDB `TransactWriteItems` constraint
  - lease fencing protocol with `LeaseToken` and version validation
  - lease renewal failure semantics: retry, step-down, release
- Add examples for both SQS modes and explicit failover behavior to `ARCHITECTURE_NEW-EXAMPLES.md`
- Add store-backend rationale to `ARCHITECTURE_NEW-MODULES.md`:
  - DynamoDB first
  - `sqliteoutbox` for local/dev
  - Postgres later, not generic SQL first
- Create `ARCHITECTURE_NEW-STORES.md` with:
  - DynamoDB table schemas for lease and outbox stores
  - conditional write expressions and GSI design
  - compaction, TTL, and hot partition mitigation strategies
  - SQS native DLQ integration strategy
  - capacity planning and IAM guidance
- Add standard header set and reserved prefix to `ARCHITECTURE_NEW.md`:
  - well-known header constants (`x-bridge.` prefix)
  - transport header mapping table
  - header validation and injection prevention rules
- Define `LeaseStore` and `OutboxStore` interfaces in `bridge/types/` (not `bridge/runtime/`)
- Add new architecture records:
  - per-route delivery mode (`direct_hold` vs `shared_outbox`)
  - DynamoDB-first shared stores with portable interfaces
  - standard header set with reserved prefix (AR-013)
  - fencing tokens on lease operations (AR-014)

## Public Contracts And Config Changes

The implementation plan should require these public contracts in the new core:

- `Envelope`, `Delivery`, `Receiver`, `Sender`, `Session`, `Lease`, `LeaseToken`
- `LeaseStore`, `OutboxStore`, `OutboxRecord`, `OutboxStatus`
- `LeaseInfo`
- `DestinationBinding`, `DispatchPlan`, `DestinationResolver`
- `SessionPlan`
- `RoutePolicy`
- `DLQEntry`, `DLQStore`
- `SessionFactory`, `ReceiverFactory`, `SenderFactory`
- Standard header constants (`HeaderCorrelationID`, `HeaderIdempotencyKey`, etc.)

Add these explicit config/runtime choices so startup validation is deterministic:

- `DeliveryMode`: `DirectHold` | `SharedOutbox`
- `DispatchMode`: `Single` | `FanOut`
- `SessionMode`: `Ephemeral` | `Persistent` | `Exclusive`
- MQTT binding options must include QoS
- Resolver/binding config must declare whether one or many dispatches are allowed

Validation rules to lock into `TASKS.md`:

- `DirectHold` requires:
  - source supports retry/visibility extension
  - `DispatchMode=Single`
  - MQTT QoS 1 or 2
  - consumer and sender are co-located on the same bridge instance
  - no lease-handoff requirement for the chosen target session
- `SharedOutbox` requires:
  - configured shared `OutboxStore`
  - configured `LeaseStore` for exclusive sessions
  - MQTT QoS 1 or 2 for reliable routes
  - all dispatch plans durably written before source ack/checkpoint for fan-out

Completion rule for MQTT outbox entries:

- QoS 1: mark `completed` on `PUBACK`
- QoS 2: mark `completed` on `PUBCOMP`
- QoS 0 is invalid for reliable outbox-backed routes
- physical deletion is async compaction after `completed`

## Isolated Implementation Tasks

`TASKS.md` should break the work into these isolated tasks with explicit dependencies.

| ID | Task | Deliverable | Depends On |
|----|------|-------------|------------|
| T0 | Align proposal docs | Proposal docs reflect both SQS delivery modes, DynamoDB-first stores, and validation rules | None |
| T1 | Create module skeletons | New mirrored modules and `go.work` entries for MQTT, SQS, DynamoDB lease/outbox, native test stores | None |
| T2 | Replace core contracts | New core ports/types/configs compile with no cloud deps | T0 |
| T3 | Build startup validator | Route/session/binding/store validation with hard-fail config errors | T2 |
| T4 | Build new runtime skeleton | Route runner, session manager, ack boundaries, replay loop, DLQ/expiry hooks using fake adapters | T2 |
| T5 | Add native test stores | `memorylease` and `sqliteoutbox` for unit/integration tests | T2 |
| T6 | Implement DynamoDB `LeaseStore` | Conditional acquire/renew/release and safe failover semantics | T2 |
| T7 | Implement DynamoDB `OutboxStore` | Partitioned persistence, claim/reclaim, complete/compact, expiry sweep | T2 |
| T8 | Port MQTT 5 adapter | Session-first MQTT adapter with reconnect, reconciliation, QoS completion, exclusive/shared modes | T2, T4 |
| T9 | Port SQS adapter | `Delivery`-based SQS receiver/sender with delete/retry/visibility extension | T2, T4 |
| T10 | Implement binding resolution | Resolver, dispatch planning, binding-driven topic/client selection, fan-out support | T2, T3 |
| T11 | Integrate `shared_outbox` path | `SQS -> outbox -> MQTT` clustered flow with session-owner replay | T6, T7, T8, T9, T10 |
| T12 | Integrate `direct_hold` path | `SQS -> MQTT` without outbox, only for co-located single-binding routes | T3, T8, T9, T10 |
| T13 | Cut over bridge construction | New runtime/factories/config path replaces the old architecture in production use | T11, T12 |
| T14 | Full verification | Module tests, integration tests, clustered failover scenarios, crash/replay acceptance | T5, T11, T12, T13 |
| T15 | CloudWatch metrics and monitoring | Custom metrics, alarms, and operational dashboards per `ARCHITECTURE_NEW-STORES.md` | T6, T7, T9 |
| T16 | HTTP API security hardening | Fix CORS defaults, require auth for admin endpoints, scope monitor endpoints, add audit logging | T13 |

Parallel work after T2:
- T3, T5, T6, T7 can run in parallel
- T8, T9, T10 can run in parallel once T4 exists

## Task-Level Requirements

`TASKS.md` should include these non-negotiable requirements per task:

- T3 must produce clear startup errors like:
  - `direct_hold invalid: resolver fan-out is enabled`
  - `shared_outbox invalid: no OutboxStore configured`
  - `shared_outbox invalid: no idempotency key processor configured and source does not guarantee Envelope.ID`
  - `shared_outbox invalid: fan-out cardinality exceeds OutboxStore transaction limit`
  - `reliable MQTT route invalid: qos=0`
- T6 must include fencing/version-safe lease semantics so a stale owner cannot keep sending:
  - `Acquire` and `Renew` must return a `LeaseToken` with a monotonically increasing `Version`
  - DynamoDB conditional writes must validate the fencing token on every lease operation
  - All `OutboxStore` mutations must accept and validate the fencing token
  - A stale token must be rejected atomically by the store
  - Renewal failure must trigger three-phase step-down: retry, stop claiming, release
- T7 must use a status model, not immediate hard delete:
  - DynamoDB table schema per `ARCHITECTURE_NEW-STORES.md`
  - atomic fan-out writes via `TransactWriteItems`
  - `replay_count` field for poison message protection with configurable max
  - strongly consistent reads for drain queries
  - TTL compaction with configurable grace periods
  - conditional writes for idempotent persist (`attribute_not_exists` on `EnvelopeID + BindingID`)
- T8 must remove local-drop behavior for reliable modes and align MQTT intake with route backpressure
- T9 must implement automatic SQS visibility timeout extension:
  - background goroutine calls `Delivery.Extend()` at 50% of visibility timeout interval
  - visibility timeout must be aligned with the processing pipeline duration
  - SQS native DLQ `maxReceiveCount` must be set higher than bridge max retries plus a safety margin of 3
- T11 must support cross-instance handoff:
  - one bridge consumes from SQS
  - another bridge owns the exclusive MQTT session
  - the message is still delivered through the correct client/topic
- T12 must explicitly reject multi-binding or leased exclusive-session routes
- T15 must emit CloudWatch custom metrics:
  - lease: acquire latency, renew latency, acquire failures, expiries
  - outbox: persist latency, drain latency, depth per partition, claim recoveries, completions
  - SQS: receive latency, delete latency, visibility extensions
  - delivery: end-to-end latency per route, DLQ entries per route and category
  - MQTT: publish latency per session, reconnects per session
  - alarms on: outbox depth thresholds, lease expiries, DLQ entries, lease acquisition failures
- T16 must address HTTP API security:
  - change CORS default from `*` to empty (require explicit configuration)
  - make API key or alternative auth mandatory for admin endpoints
  - move sensitive monitor endpoints (traces, topology, logs) behind authentication
  - add structured audit logging for lease transitions, DLQ operations, and admin API calls

## Test Plan

`TASKS.md` should require automated coverage for:

- `SQS -> MQTT` in `direct_hold`
- `SQS -> MQTT` in `shared_outbox`
- `MQTT -> SQS`
- one SQS queue to many MQTT named clients with binding resolution
- one source message fan-out to multiple MQTT clients
- exclusive MQTT client failover between bridge instances
- crash:
  - before outbox persist
  - after outbox persist but before SQS delete
  - after SQS delete but before MQTT send
  - after `PUBACK`/`PUBCOMP` but before outbox completion persistence
- expired outbox entry dropped before replay
- stale lease recovery
- replay reclaim after sender crash
- startup validation failures for invalid route combinations
- fencing token validation: stale owner rejected on outbox claim after lease transfer
- poison message moved to DLQ after exceeding max replay count
- fan-out atomicity: partial outbox persist followed by crash does not create orphan records
- idempotent outbox persist: redelivered source message does not create duplicate outbox entries
- header injection prevention: reserved-prefix headers stripped from external sources

## Assumptions And Defaults

- `TASKS.md` lives in `bridge/types/` beside the architecture docs
- Phase 1 is AWS-first and MQTT-5-first
- DynamoDB is the first production shared store because it matches the chosen platform and existing repo/module direction
- A SQL-like store is not the first production backend here; if production SQL is added later, choose Postgres explicitly
- `sqliteoutbox` is phase-1 local/dev/test infrastructure, not the primary clustered store
- No long-term compatibility layer for the old runtime is required; this is a major cutover plan
