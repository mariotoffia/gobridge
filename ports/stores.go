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
//
// Freshness invariant (single-active safety): a successful Acquire or Renew MUST
// persist a lease expiry (observable as LeaseInfo.ExpiresAt) that is NO EARLIER
// than the wall-clock instant the client BEGAN the call plus ttl. The session
// manager derives its own voluntary step-down deadline from the pre-call
// timestamp + ttl and stops acting as owner once that deadline passes; if a
// store persisted a SHORTER effective lifetime than the caller's start+ttl, a
// competing instance could acquire the lease while the current owner still
// believes it holds it — split-brain. Persisting a LATER expiry (server clock
// ahead of the client, or an added safety margin) is always safe.
//
// Renew MUST NOT bump the returned token's Version: a renewal preserves the
// fencing version established at Acquire. A Renew that advanced the version
// would fence the owner's OWN earlier in-flight claims (which carry the
// pre-renewal version) below the new high-water-mark, causing an owner to
// stale out its own outstanding work. Only Acquire (a fresh ownership epoch)
// advances the version.
type LeaseStore interface {
	Acquire(ctx context.Context, leaseID string, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
	Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
	Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error
	Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error)
}

// ManagedSubscriptionStore persists the exact topic-filter history owned by
// durable transport sessions. The storageIdentity is a secret-safe durable
// identity, never a broker URL, credential, or mutable display name.
//
// List returns a lexicographically sorted, duplicate-free snapshot. It returns
// shared.ErrNotFound when no baseline has ever been established for the
// identity; an established empty baseline returns an empty slice and nil.
// Remember atomically establishes the baseline and adds every filter.
// Remember(identity, nil) is therefore the explicit empty-baseline operation.
// Forget atomically removes the listed filters while preserving the baseline;
// unknown filters and Forget(identity, nil) are idempotent no-ops, but an
// identity without a baseline returns shared.ErrNotFound.
//
// All operations reject an empty identity or any empty filter with
// shared.ErrInvalidConfig before mutation. Implementations must honor context
// cancellation and must not expose storageIdentity or filters in error text.
type ManagedSubscriptionStore interface {
	List(ctx context.Context, storageIdentity string) ([]string, error)
	Remember(ctx context.Context, storageIdentity string, filters []string) error
	Forget(ctx context.Context, storageIdentity string, filters []string) error
}

// ManagedSubscriptionStoreFactory is the optional store-factory capability for
// exact durable subscription history. It remains separate from StoreFactory so
// lease/outbox/DLQ-only plugins are not forced to implement an unrelated port.
type ManagedSubscriptionStoreFactory interface {
	NewManagedSubscriptionStore(ctx context.Context, cfg PluginConfig) (ManagedSubscriptionStore, error)
}

// DLQReader is the read-side of the dead-letter queue: lookups and
// scans that never mutate stored entries. Driving read adapters (the
// runtime read port, monitor endpoints) depend on this narrow port so
// they cannot delete or purge.
//
// List ordering contract: entries are returned OLDEST-FIRST — ascending
// FailedAt with ascending entry ID as the deterministic tiebreaker — so
// operators triage the oldest failures first and pagination via
// DLQFilter.Limit walks forward in time identically on every backend.
type DLQReader interface {
	Get(ctx context.Context, id string) (routing.DLQEntry, error)
	List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error)
}

// DLQAdmin is the write/administration side of the dead-letter queue:
// ingest plus the destructive operations (delete, delete-by-filter,
// purge). Only the runtime command port and admin endpoints depend on
// it, keeping mutation authority off the read path.
//
// DeleteByFilter deletes EVERY entry matching the filter when
// DLQFilter.Limit <= 0, regardless of any internal scan page size, and
// returns the accurate deleted count. With a positive Limit it deletes at
// most Limit entries, selected oldest-first per the DLQReader ordering
// contract.
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

// DLQDepthReporter is an OPTIONAL dead-letter-queue capability that returns the
// CURRENT number of outstanding entries — the standing DLQ backlog. It is the
// read primitive behind shared.MetricDLQDepth (sampled via
// runtime.ReportDLQDepth): the existing DLQEntries counter only ever COUNTS
// writes and never decreases, so a stale backlog after a burst (writes have
// since stopped, nothing redriven) is invisible without a manual storage scan.
// Depth closes that gap so operators can alarm on "records sitting in the DLQ
// right now".
//
// Contract:
//
//   - Depth returns the total number of entries currently stored (across all
//     routes/categories). It never mutates. A fleet total keeps metric
//     cardinality low and matches the dimensionless default rollup alarm; a
//     per-route split, if ever needed, belongs on a separate method so the
//     default depth series stays low cardinality.
//   - Back it by an efficient COUNT / item-count metadata read, not by paging
//     every entry — it is sampled periodically, so cost matters.
//   - A transient backend error is returned as-is; callers treat it as "depth
//     unavailable this sample" and emit nothing.
//
// OPTIONAL: DLQ adapters that do not implement it simply expose no DLQ-depth
// gauge (runtime.ReportDLQDepth is a no-op), with no build or behaviour break.
type DLQDepthReporter interface {
	Depth(ctx context.Context) (int, error)
}

// OutboxRuntimeOptions carries runtime tuning knobs the bridge
// derives from the blueprint and threads through to outbox factories
// without polluting the typed plugin config. StaleClaimDuration is
// derived from the maximum session step-down grace and bounds how
// long a claimed-but-not-completed outbox record waits before
// another owner can reclaim it.
//
// Metrics is the runtime's MetricsExporter (the same one handed to
// routes) so a store backend can emit store-level observability signals
// such as MetricOutboxClaimConflicts. It is nil when the deployment
// configured no exporter; factories MUST treat nil as "no metrics" and
// substitute a no-op rather than dereferencing it.
type OutboxRuntimeOptions struct {
	StaleClaimDuration time.Duration
	Metrics            MetricsExporter
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
