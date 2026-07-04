package ports

import "context"

// TenantInfo holds the subset of tenant metadata required by the
// tenant processor for validation and quota enforcement.
type TenantInfo struct {
	ID                  string
	Active              bool
	MaxMessageSizeBytes int64
}

// TenantValidator resolves and validates a tenant by ID. Implementations
// must be safe for concurrent use from multiple goroutines.
type TenantValidator interface {
	Validate(ctx context.Context, tenantID string) (TenantInfo, error)
}

// TenantUsageTracker records per-tenant usage counters. Implementations
// must be safe for concurrent use from multiple goroutines.
//
// ponytail: increment-only by design. Hard quota ceilings (reject when a
// tenant's message count exceeds a limit) would need a read-back extension
// (e.g. a usage query returning current counters). Deferred until a consumer
// exists: processors/tenant only observes usage today, and an unconsumed
// port method is dead contract surface every implementation must carry.
// When quota enforcement lands, add a separate optional extension interface
// (e.g. TenantUsageReader) alongside this one rather than widening it, so
// existing trackers keep compiling.
type TenantUsageTracker interface {
	IncrementMessages(ctx context.Context, tenantID string, count int64) error
	IncrementInFlight(ctx context.Context, tenantID string, delta int64) error
}
