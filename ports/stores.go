package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
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
//
// Fencing contract:
//
//   - Claim fencing is version-monotonic. A record is claimable when it is
//     pending, or when it is claimed and its claim_version is strictly older
//     than LeaseToken.Version (the previous owner's lease was preempted).
//     Stores MUST honour this version-older rule. A store MAY ADDITIONALLY
//     reclaim a claimed record whose claim has gone stale past a wall-clock
//     staleness threshold — a crash-recovery fallback for an owner that died
//     without bumping the version. The DynamoDB backend implements this
//     time-stale fallback; the memory and SQLite backends are version-only.
//     On a successful claim the store sets claimed_by from token.Owner; there
//     is no separate owner parameter, so token.Owner is the single source of
//     claim authority.
//   - Completion fencing is owner+version+status. Complete may transition a
//     record only when it is currently claimed, claimed_by == token.Owner, and
//     claim_version == token.Version. On any mismatch the store MUST return
//     shared.ErrStaleFencingToken rather than silently skipping the record.
//   - Expire is pending-only. It may transition to expired only records that
//     are still pending with a non-zero expires_at strictly before the cutoff.
//     Claimed records are never expired here; a claimed-but-stale record is
//     reclaimed through Claim/IsClaimable, never expired out from under a
//     potentially still-valid owner.
//
// Lease binding: the runtime TokenFn lease gate is the authoritative access
// control for who may claim and complete. An OutboxStore validates the
// fencing token only against the record's own claim metadata; it is NOT
// required to consult the current LeaseStore state. Cross-store consultation
// of the live lease is explicitly OUT OF SCOPE of this port, because the
// outbox and lease stores may be backed by different systems and need not
// share a consistency boundary.
type OutboxStore interface {
	Persist(ctx context.Context, records []*persistence.OutboxRecord) error
	Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error)
	Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
	Expire(ctx context.Context, before time.Time) (int, error)
	QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error)
}

// DLQReader is the read-side of the dead-letter queue: lookups and
// scans that never mutate stored entries. Driving read adapters (the
// runtime read port, monitor endpoints) depend on this narrow port so
// they cannot delete or purge.
type DLQReader interface {
	Get(ctx context.Context, id string) (routing.DLQEntry, error)
	List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error)
}

// DLQAdmin is the write/administration side of the dead-letter queue:
// ingest plus the destructive operations (delete, delete-by-filter,
// purge). Only the runtime command port and admin endpoints depend on
// it, keeping mutation authority off the read path.
type DLQAdmin interface {
	Write(ctx context.Context, entry routing.DLQEntry) error
	Delete(ctx context.Context, ids []string) (int, error)
	DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error)
	Purge(ctx context.Context, before time.Time) (int, error)
}

// DLQStore manages dead-letter queue entries for failed or rejected
// messages. It is the full driven port store adapters implement; it
// composes the read and admin halves so a single adapter satisfies
// DLQReader, DLQAdmin, and DLQStore at once.
type DLQStore interface {
	DLQReader
	DLQAdmin
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
