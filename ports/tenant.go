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
type TenantUsageTracker interface {
	IncrementMessages(ctx context.Context, tenantID string, count int64) error
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
