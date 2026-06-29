// Package events defines the observability events GoBridge emits about
// its bounded contexts: persistence (outbox lifecycle, lease fencing),
// routing (DLQ ingress and redrive), connectivity (credential
// rotation), and configuration (blueprint commit).
//
// These are application/runtime FACTS, not aggregate-raised domain
// events. Domain aggregates (OutboxRecord, the lease, DLQ entries, the
// blueprint) do NOT record or return events on their transitions;
// instead the application and runtime services that drive those
// transitions construct an Event via the public constructors in this
// package (for example NewOutboxRecordClaimed) once a transition has
// succeeded, and publish it through ports.EventPublisher on a
// best-effort basis. The events are therefore immutable past-tense
// facts emitted beside the work, not a side effect of the aggregate.
//
// Each event carries a stable EventID, a namespaced EventType, the wall
// time at which the fact occurred, the AggregateID of the aggregate the
// fact concerns, and a SchemaVersion that follows semantic versioning
// so downstream consumers can evolve independently of producers.
//
// Layering. This package is part of the innermost (Layer 1) ring of
// the architecture and depends on the standard library only. It does
// not import any other domain bounded context: events carry primitive
// values, not aggregate references, so that consumers can deserialize
// them without dragging the producing context into their dependency
// graph. The companion port `ports.EventPublisher` is the egress
// boundary; adapters (audit log, message bus, durable outbox) live in
// outer rings and consume Event values without ever touching producer
// internals.
//
// Naming. Event types follow the convention
// `<bounded-context>.<aggregate>.<verb-past>`, e.g.
// `persistence.outbox.claimed`. The `EventType()` accessor returns
// the canonical string; callers MUST NOT branch on string literals
// constructed at the call site -- compare against the SchemaXxx
// constants exported from this package instead.
package events
