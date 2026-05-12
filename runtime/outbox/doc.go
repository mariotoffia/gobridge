// Package outbox owns the runtime's outbox dispatch machinery: the
// per-partition Drainer that claims pending [persistence.OutboxRecord]s,
// validates lease fencing tokens, sends through the target
// [ports.Sender], and routes terminal failures to the DLQ; the
// DepthCache that short-circuits repeated QueryPending calls on the
// shared-outbox hot path; and the timeout-scaling helper that derives
// per-batch deadlines from the configured per-record budget.
//
// This package is a leaf within the runtime layer. It depends only on
// inward layers (`domain/*`, `ports`) and the cross-cutting `logging`
// utility plus the sibling `runtime/dlq` leaf. The parent `runtime`
// package composes a *outbox.Drainer per shared-outbox session and
// consumes it through the standard inward dependency rule
// (parent -> leaf). This leaf MUST NOT depend on its parent
// (`runtime`), on any adapter, on bridge, or on the composition root.
// Treat any new outward edge here as a smell that the leaf is
// absorbing orchestration concerns it should publish back to runtime
// instead.
//
// The [persistence.OutboxRecord] aggregate itself remains in
// `domain/persistence`; this package owns only the runtime-side
// plumbing that operates on it.
package outbox
