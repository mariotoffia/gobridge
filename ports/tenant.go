package ports

import "context"

// TenantInfo holds the subset of tenant metadata required by the
// tenant processor for validation and quota enforcement.
type TenantInfo struct {
	ID                  string
	Active              bool
	MaxMessageSizeBytes int64
	// MaxInFlight caps the number of concurrently in-flight deliveries a
	// tenant may hold. 0 means unlimited, matching the zero-means-off
	// convention of MaxMessageSizeBytes. The tenant processor enforces it
	// only when the usage tracker also implements TenantUsageReader.
	MaxInFlight int64
}

// TenantValidator resolves and validates a tenant by ID. Implementations
// must be safe for concurrent use from multiple goroutines.
type TenantValidator interface {
	Validate(ctx context.Context, tenantID string) (TenantInfo, error)
}

// TenantUsageTracker records per-tenant usage counters. Implementations
// must be safe for concurrent use from multiple goroutines.
//
// This interface stays increment-only by design. Point-in-time read-back
// (needed for quota enforcement) lives in the optional TenantUsageReader
// extension below rather than widening this one, so existing
// increment-only trackers keep compiling.
//
// # In-flight crash-decay contract (HARD CONTRACT)
//
// A tracker whose state is SHARED across bridge instances (e.g. a Redis or
// DynamoDB counter used to enforce a cross-instance MaxInFlight ceiling) MUST
// bound its in-flight counts against instance death. The tenant processor
// brackets each delivery with a paired IncrementInFlight(+1) / (-1): the +1
// lands as the delivery is admitted and the -1 runs once the delivery settles.
// If the instance is killed (kill -9, OOM, hardware loss) between the two, the
// +1 is durable but the -1 never runs — the tenant's shared in-flight count is
// permanently inflated. Enough such leaks and the tenant is throttled forever
// (usage.InFlight stays >= MaxInFlight) with no path back to deliverable.
//
// A conforming SHARED tracker therefore MUST make each in-flight increment
// self-healing rather than eternal — for example by modelling each +1 as a
// TTL-leased item the store auto-expires (so a crashed instance's counts decay
// on their own), or by implementing the optional TenantUsageReconciler below so
// the embedder can actively reap stranded counts on instance restart / lease
// takeover. A purely additive shared counter with no decay is NOT a conforming
// implementation. (A per-instance / in-memory tracker is exempt: its counts die
// with the process.)
type TenantUsageTracker interface {
	IncrementMessages(ctx context.Context, tenantID string, count int64) error
	// IncrementInFlight adjusts the tenant's in-flight delivery count by delta
	// (+1 on admission, -1 on settle). See the type-level crash-decay contract:
	// a SHARED implementation MUST bound the +1 against instance death (TTL
	// lease or TenantUsageReconciler) so a crash between the +1 and its paired
	// -1 does not strand the tenant over-quota permanently.
	IncrementInFlight(ctx context.Context, tenantID string, delta int64) error
}

// TenantUsage is a point-in-time snapshot of a tenant's counters.
type TenantUsage struct {
	Messages int64 // total messages observed (monotonic)
	InFlight int64 // currently in-flight deliveries
}

// TenantUsageReader is an optional extension of TenantUsageTracker.
// Implementations that can answer point-in-time usage queries implement
// it alongside the tracker; the tenant processor type-asserts for it and
// enforces quota ceilings only when both the reader capability AND a
// non-zero ceiling on TenantInfo are present. Kept separate from
// TenantUsageTracker so existing increment-only trackers keep compiling.
type TenantUsageReader interface {
	Usage(ctx context.Context, tenantID string) (TenantUsage, error)
}

// TenantUsageReconciler is an optional extension a shared TenantUsageTracker
// MAY implement to actively reap in-flight counts stranded by a crashed
// instance, as an alternative to passive TTL decay (see the TenantUsageTracker
// crash-decay contract). The embedder calls ReconcileInFlight on instance
// restart / lease takeover; it returns the number of stranded in-flight units
// reclaimed for the tenant.
//
// ponytail: this is a documented EXTENSION POINT only — the runtime deliberately
// wires no epoch/reconcile machinery to it, because no tracker ships with
// GoBridge (trackers are operator-supplied) and coupling the runtime to a
// hypothetical reconcile lifecycle would be speculative. An operator that ships
// a shared tracker implements this (or TTL decay) and drives it from their own
// instance-lifecycle hook.
type TenantUsageReconciler interface {
	ReconcileInFlight(ctx context.Context, tenantID string) (reaped int64, err error)
}
