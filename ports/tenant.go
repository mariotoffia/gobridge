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
type TenantUsageTracker interface {
	IncrementMessages(ctx context.Context, tenantID string, count int64) error
	IncrementInFlight(ctx context.Context, tenantID string, delta int64) error
}
