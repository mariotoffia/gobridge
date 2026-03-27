package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// LeaseStore manages distributed lease ownership for single-active scenarios.
// Implementations must use conditional writes to enforce fencing semantics.
//
// The endpoints parameter on Acquire and Renew stores the owner's reachable
// addresses alongside the lease record. Other instances retrieve these via
// Current to discover how to reach the lease owner for cluster-aware routing.
type LeaseStore interface {
	Acquire(ctx context.Context, leaseID string, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error)
	Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error)
	Release(ctx context.Context, leaseID string, token domain.LeaseToken) error
	Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error)
}

// OutboxStore manages the durable outbox for reliable egress.
// All mutations that accept a LeaseToken must validate the fencing token
// and reject stale tokens atomically.
type OutboxStore interface {
	Persist(ctx context.Context, records []domain.OutboxRecord) error
	Claim(ctx context.Context, partitionKey string, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error)
	Complete(ctx context.Context, recordIDs []string, token domain.LeaseToken) error
	Expire(ctx context.Context, before time.Time) (int, error)
	QueryPending(ctx context.Context, partitionKey string, limit int) ([]domain.OutboxRecord, error)
}

// DLQStore manages dead-letter queue entries for failed or rejected messages.
type DLQStore interface {
	Write(ctx context.Context, entry domain.DLQEntry) error
	List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error)
	Replay(ctx context.Context, entryIDs []string) error
	Purge(ctx context.Context, before time.Time) (int, error)
}
