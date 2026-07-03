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
// Persist idempotency contract:
//
//   - Persist is idempotent per record. A record's persistence identity is
//     (partition key, EnvelopeID, BindingID). Records whose identity already
//     exists in the store are SKIPPED — not overwritten, not an error — while
//     every new record in the same batch IS persisted. This makes fan-out
//     re-persist after a partial failure safe: already-persisted legs are
//     no-ops and previously-unpersisted legs are stored.
//   - Persist returns shared.ErrDuplicateRecord ONLY when every record in
//     the batch already existed (nothing was persisted). Callers use this
//     purely as a signal that the whole batch was a replay; it is not a
//     failure of durability.
//   - A batch that contains the same identity twice persists the first
//     occurrence and skips the rest, following the same per-record rule.
//
// Claim ordering contract:
//
//   - Claim returns records in per-partition persist order: ascending
//     (CreatedAt, Seq), where Seq is a monotonic per-partition sequence the
//     store assigns at Persist. Records persisted before the sequence existed
//     carry Seq 0 and sort first within their CreatedAt millisecond, which
//     preserves their relative age. Under a backlog deeper than `limit`, Claim
//     SELECTS the oldest-N pending records (not an arbitrary subset), because
//     per-partition send ordering depends on it.
//   - QueryPending returns records ordered ascending (CreatedAt, Seq) WITHIN
//     the returned set, but its SELECTION under a backlog deeper than `limit`
//     is store-defined: it is a depth/preview query (the runtime uses it only
//     to count pending records against MaxOutboxDepth), so a store MAY return
//     the first `limit` records in its native scan order rather than the
//     globally oldest-N. The in-memory and SQLite backends happen to select
//     oldest-N (ORDER BY created_at, seq LIMIT); the DynamoDB backend selects
//     in SK order for depth/preview to avoid read amplification. Callers that
//     need oldest-N SELECTION must use Claim, never QueryPending.
//   - Claim with limit <= 0 is a fencing no-op: it validates the token and
//     advances the durable per-partition fencing high-water-mark exactly
//     like an empty-partition claim, but claims and returns no records.
//   - Stores MUST NOT filter claimable records by replay count. Poison
//     detection (max replay attempts) is the drainer's decision; a store
//     that hides high-replay records from Claim makes them unreachable for
//     DLQ routing.
//
// Fencing contract:
//
//   - Claim fencing is version-monotonic. A record is claimable when it is
//     pending, or when it is claimed and its claim_version is strictly older
//     than LeaseToken.Version (the previous owner's lease was preempted).
//     Stores MUST honour this version-older rule. A store MAY ADDITIONALLY
//     reclaim a claimed record whose claim has gone stale past a wall-clock
//     staleness threshold — a crash-recovery fallback for an owner that died
//     without bumping the version. The DynamoDB and SQLite backends implement
//     this time-stale fallback; the in-memory backend is version-only.
//     On a successful claim the store sets claimed_by from token.Owner; there
//     is no separate owner parameter, so token.Owner is the single source of
//     claim authority.
//   - Claim maintains a DURABLE per-partition fencing high-water-mark: the
//     highest token.Version observed on ANY Claim, including a Claim that
//     returns no records (a no-op claim against an empty or fully-claimed
//     partition still advances it). A Claim whose token.Version is strictly
//     below this high-water-mark MUST be rejected with shared.ErrStaleFencingToken
//     so a preempted owner cannot win freshly-arrived pending work that lands
//     after a higher-version owner has taken over the partition.
//   - Completion fencing is owner+version+status. Complete may transition a
//     record only when it is currently claimed, claimed_by == token.Owner, and
//     claim_version == token.Version. On any mismatch the store MUST return
//     shared.ErrStaleFencingToken rather than silently skipping the record.
//     Backends that resolve record IDs through an eventually-consistent
//     secondary index (DynamoDB RecordIDIndex) may transiently fail to find
//     a just-written record after a process restart evicts the key cache;
//     they retry with backoff and surface shared.ErrTimeout if the index
//     never converges, leaving the record claimed until stale reclaim. The
//     live drainer only completes records it claimed in-process, so this
//     window exists only across restarts.
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

// OutboxReleaser is an OPTIONAL OutboxStore capability. A store that
// implements it lets a still-alive owner return a transiently-failed
// claimed record to pending immediately, so it is re-claimable on the
// next drain without a fencing-version bump or a wall-clock stale-claim
// timeout. Stores that do NOT implement it fall back to version/stale
// reclaim; on such a store a live owner cannot retry until its lease
// version advances.
//
// Release fencing is owner+version+status, identical to Complete: it
// transitions a record only when it is currently claimed,
// claimed_by == token.Owner, and claim_version == token.Version. On any
// mismatch the store MUST return shared.ErrStaleFencingToken rather than
// silently skipping the record.
//
// Release is single-record-intended: the live drainer always passes exactly
// one recordID. The recordIDs slice is retained for signature symmetry with
// Complete. The memory and SQLite backends validate the whole batch before
// mutating (all-or-nothing); DynamoDB releases per-record and stops at the
// first mismatch, so earlier ids may already be released. Pass one id to
// stay within the well-defined single-record contract.
//
// Release is claim-scoped, not idempotent: it only acts on a currently
// claimed record. Re-releasing an already-pending (or completed) record is a
// status mismatch and returns shared.ErrStaleFencingToken.
type OutboxReleaser interface {
	Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
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
