// Package route owns the runtime's per-route ingress pipeline: the
// processor chain that wraps each delivery, the [RouteRunner] that
// drives the configured [ports.Receiver], and the dispatch path that
// resolves [routing.DispatchPlan]s, renders binding addresses, and
// either sends directly (DirectHold) or persists into the
// [persistence.OutboxStore] (SharedOutbox). Per-record retry/backoff,
// processor panic/timeout recovery, DLQ classification, and idle
// signalling all live here.
//
// This package is a leaf within the runtime layer. It depends only on
// inward layers (`domain/*`, `ports`), cross-cutting utilities
// (`logging`, `observability`), and the sibling runtime leaves
// `runtime/dlq` and `runtime/outbox`. The parent `runtime` package
// composes a *route.RouteRunner per [RouteConfig] and consumes it
// through the standard inward dependency rule (parent -> leaf). This
// leaf MUST NOT depend on its parent (`runtime`), on any sibling
// `runtime/session`, `runtime/cluster`, or `runtime/credentials`
// package, on any adapter, on bridge, or on the composition root.
// Treat any new outward edge here as a smell that the leaf is
// absorbing orchestration concerns it should publish back to runtime
// instead.
//
// Aggregate types ([persistence.OutboxRecord], [routing.RoutePolicy],
// [routing.DispatchPlan]) remain in their `domain/*` contexts; this
// package owns only the runtime-side execution plumbing that operates
// on them.
package route
