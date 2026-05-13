// Package events defines the first-wave domain events emitted by the
// GoBridge bounded contexts: persistence (outbox lifecycle, lease
// fencing), routing (DLQ ingress and redrive), connectivity
// (credential rotation), and the configuration aggregate (blueprint
// commit).
//
// Domain events in this package are immutable past-tense facts. Each
// event carries a stable EventID, a namespaced EventType, the wall
// time at which the fact occurred, the AggregateID of the producing
// aggregate, and a SchemaVersion that follows semantic versioning so
// downstream consumers can evolve independently of producers.
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
