// Package dlq owns the runtime's dead-letter-queue routing leaf: the
// asynchronous [Router] that classifies failed deliveries, builds
// [routing.DLQEntry] values, and persists them through the
// [ports.DLQStore] port either synchronously or via a buffered
// background pipeline with bounded retry/backoff (classify → enqueue →
// drain).
//
// This package is a leaf within the runtime layer. It depends only on
// inward layers (`domain/*`, `ports`) and carries no transport,
// storage, or composition-root concerns. The parent `runtime` package
// composes a *dlq.Router and consumes it through the standard inward
// dependency rule (parent -> leaf). This leaf MUST NOT depend on its
// parent (`runtime`), on any sibling runtime leaf, on any adapter, on
// bridge, or on the composition root. Treat any new outward edge here
// as a smell that the leaf is absorbing orchestration concerns it
// should publish back to runtime instead.
package dlq
