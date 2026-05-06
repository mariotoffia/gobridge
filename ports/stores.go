package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/persistence"
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
	Acquire(ctx context.Context, leaseID string, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
	Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
	Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error
	Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error)
}

// OutboxStore manages the durable outbox for reliable egress.
// All mutations that accept a LeaseToken must validate the fencing token
// and reject stale tokens atomically.
type OutboxStore interface {
	Persist(ctx context.Context, records []persistence.OutboxRecord) error
	Claim(ctx context.Context, partitionKey string, ownerID string, token persistence.LeaseToken, limit int) ([]persistence.OutboxRecord, error)
	Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
	Expire(ctx context.Context, before time.Time) (int, error)
	QueryPending(ctx context.Context, partitionKey string, limit int) ([]persistence.OutboxRecord, error)
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

// OutboxRuntimeOptions carries runtime tuning knobs the bridge
// derives from the blueprint and threads through to outbox factories
// without polluting the typed plugin config. StaleClaimDuration is
// derived from the maximum session step-down grace and bounds how
// long a claimed-but-not-completed outbox record waits before
// another owner can reclaim it.
type OutboxRuntimeOptions struct {
	StaleClaimDuration time.Duration
}

// StoreFactory creates backing store instances (lease, outbox, DLQ)
// from a typed PluginConfig. A factory should return (nil, nil) for
// a store role it does not handle. Implementations type-assert cfg
// to their concrete config; cfg may be nil when the user supplied no
// options block in the blueprint.
type StoreFactory interface {
	NewLeaseStore(ctx context.Context, cfg PluginConfig) (LeaseStore, error)
	NewOutboxStore(ctx context.Context, cfg PluginConfig, runtime OutboxRuntimeOptions) (OutboxStore, error)
	NewDLQStore(ctx context.Context, cfg PluginConfig) (DLQStore, error)
}

// DistributedStoreFactory is an optional interface that StoreFactory
// implementations may satisfy to declare whether the underlying store
// provides cross-process coordination. Factories that do not implement
// this interface are assumed to be process-local (not distributed).
type DistributedStoreFactory interface {
	IsDistributed() bool
}
