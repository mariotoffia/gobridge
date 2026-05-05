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
//
// Renew must return ErrStaleFencingToken if the lease has already expired.
// A paused owner must not be able to silently re-establish an expired lease;
// it must re-acquire through Acquire instead.
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
	Get(ctx context.Context, id string) (domain.DLQEntry, error)
	List(ctx context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error)
	Delete(ctx context.Context, ids []string) (int, error)
	DeleteByFilter(ctx context.Context, filter domain.DLQFilter) (int, error)
	Purge(ctx context.Context, before time.Time) (int, error)
}

// StoreSpec carries the generic shape of a store configuration as
// produced by the bridge from ports.StoreConfig. The typed plugin
// config (post-stage-2 decoding) is in Config; PHASE3 will remove the
// legacy Options carrier entirely.
type StoreSpec struct {
	Type    string
	Config  PluginConfig
	Options map[string]any
}

// StoreFactory creates backing store instances (lease, outbox, DLQ)
// from a generic StoreSpec. A factory should return (nil, nil) for a
// store role it does not handle.
type StoreFactory interface {
	NewLeaseStore(ctx context.Context, spec StoreSpec) (LeaseStore, error)
	NewOutboxStore(ctx context.Context, spec StoreSpec) (OutboxStore, error)
	NewDLQStore(ctx context.Context, spec StoreSpec) (DLQStore, error)
}

// DistributedStoreFactory is an optional interface that StoreFactory
// implementations may satisfy to declare whether the underlying store
// provides cross-process coordination. Factories that do not implement
// this interface are assumed to be process-local (not distributed).
type DistributedStoreFactory interface {
	IsDistributed() bool
}
