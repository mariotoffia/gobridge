package ports

import (
	"context"
	"errors"
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
//   - Every guarded mutation (Claim, Complete, and OutboxReleaser.Release)
//     REQUIRES a valid fencing token: token.Owner != "" AND token.Version > 0
//     (persistence.LeaseToken.Valid). A store MUST reject a zero-value or
//     otherwise invalid persistence.LeaseToken{} with shared.ErrStaleFencingToken
//     BEFORE performing any state transition or advancing the per-partition
//     fencing high-water-mark. A zero-value token is never a real lease:
//     accepting one would let a miswired or buggy drainer claim or complete
//     work without lease ownership, defeating clustering/fencing. (Expire takes
//     no token; it is authorised solely by the caller's lease over the supplied
//     partition.)
//
//     This rejection is on the token's OWN validity and is INDEPENDENT of the
//     stored row's shape: it MUST fire before — and regardless of whether — the
//     token matches the record's stored claim metadata. A corrupt or
//     pre-fencing "zero-claimed" row (claimed_by == "" AND claim_version == 0)
//     would otherwise be fraudulently MATCHED by a zero-value LeaseToken{} under
//     a naive owner+version+status comparison and be completed or released by a
//     caller that never held a lease. Stores that route the transition through
//     the OutboxRecord aggregate inherit this guard (the aggregate refuses to
//     Complete/Release a Claimed row whose stored claim identity is itself
//     invalid); stores that mutate via raw SQL / conditional writes MUST add the
//     token-validity check explicitly.
//
//   - Claim fencing is version-monotonic. A record is claimable when it is
//     pending, or when it is claimed and its claim_version is strictly older
//     than LeaseToken.Version (the previous owner's lease was preempted).
//     Stores MUST honour this version-older rule. A store MAY ADDITIONALLY
//     reclaim a claimed record whose claim has gone stale past a wall-clock
//     staleness threshold — a crash-recovery fallback for an owner that died
//     without bumping the version, so it MAY reclaim a claim at the SAME
//     version (not only a strictly-older one). The DynamoDB and SQLite
//     backends implement this time-stale fallback; the in-memory backend is
//     version-only. This equal-version, time-stale reclaim is the one
//     sanctioned bypass of OutboxRecord.Claim's strictly-older rule; stores
//     effect it through persistence.RehydrateFromSnapshot (see its godoc).
//     On a successful claim the store sets claimed_by from token.Owner; there
//     is no separate owner parameter, so token.Owner is the single source of
//     claim authority.
//
//     Wall-clock semantics: the staleness threshold is compared against
//     wall-clock time (the store's own clock versus the claim's persisted
//     claimed_at). Across a cluster these clocks drift, so the threshold MUST
//     DOMINATE the worst-case cluster clock skew: a node whose clock runs
//     fast MUST NOT reclaim a claim that a still-live owner refreshed only
//     moments ago. Operators set the threshold strictly GREATER than the
//     maximum tolerated skew between nodes (threshold > max_skew, with margin
//     for the owner's refresh interval); a threshold at or below max skew
//     lets a skewed node preempt a live owner early and duplicate its work.
//
//   - Claim maintains a DURABLE per-partition fencing high-water-mark: the
//     highest token.Version observed on ANY Claim, including a Claim that
//     returns no records (a no-op claim against an empty or fully-claimed
//     partition still advances it). A Claim whose token.Version is strictly
//     below this high-water-mark MUST be rejected with shared.ErrStaleFencingToken
//     so a preempted owner cannot win freshly-arrived pending work that lands
//     after a higher-version owner has taken over the partition.
//
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
//
//   - Complete batch atomicity. The live drainer always passes exactly one
//     recordID, so Complete is single-record in practice; the slice exists for
//     signature symmetry with Claim's output. When more than one id is passed
//     the batch is NOT guaranteed all-or-nothing across backends. Only the
//     memory store validates the whole batch before mutating (all-or-nothing).
//     SQLite issues one filter-UPDATE that completes EVERY matching record and
//     then returns shared.ErrStaleFencingToken if any id failed its fence — the
//     matched ids stay completed. DynamoDB completes per-record and stops at the
//     first fencing mismatch, so earlier ids are already completed when a later
//     one returns shared.ErrStaleFencingToken. In short: SQLite and DynamoDB may
//     both leave already-matched records completed when a sibling id in the batch
//     fails. Callers that need a definite terminal outcome per record MUST pass
//     one id at a time (as the drainer does) rather than infer atomicity from a
//     single returned error.
//
//   - Expire is pending-only AND partition-scoped. It may transition to expired
//     only records that are still pending with a non-zero expires_at strictly
//     before the cutoff AND whose partition equals the supplied partition. The
//     partition is REQUIRED — there is no magic empty-string "all partitions"
//     value: the sweep is authorized by the caller's lease over that one
//     partition, so it MUST NOT cross into partitions the caller does not own
//     (M1). Claimed records are never expired here; a claimed-but-stale record
//     is reclaimed through Claim/IsClaimable, never expired out from under a
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
	Expire(ctx context.Context, before time.Time, partition string) (int, error)
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
// A zero-value / invalid token (persistence.LeaseToken.Valid == false) is
// rejected with shared.ErrStaleFencingToken: it can never match a real claim's
// non-empty owner and non-zero version.
//
// Release is single-record-intended: the live drainer always passes exactly
// one recordID. The recordIDs slice is retained for signature symmetry with
// Complete. Only the memory backend validates the whole batch before mutating
// (all-or-nothing); SQLite issues one filter-UPDATE (every matching record is
// released, then shared.ErrStaleFencingToken if any id failed its fence) and
// DynamoDB releases per-record stopping at the first mismatch, so on both,
// earlier matched ids may already be released when a sibling fails. Pass one id
// to stay within the well-defined single-record contract.
//
// Release is claim-scoped, not idempotent: it only acts on a currently
// claimed record. Re-releasing an already-pending (or completed) record is a
// status mismatch and returns shared.ErrStaleFencingToken.
type OutboxReleaser interface {
	Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
}

// OutboxDepthReporter is an OPTIONAL OutboxStore capability that returns the
// EXACT (or store-estimated) number of PENDING records for a partition — the
// true backlog depth, independent of any claim batch size. It exists because
// the drain path otherwise only knows how many records it just CLAIMED (a value
// that saturates at the batch ceiling and cannot distinguish a deep backlog
// from a full batch), and QueryPending is a record-materialising preview query
// bounded by its limit, not an efficient count. A store that implements this
// lets the drainer emit shared.MetricOutboxDepth as a true, unbounded backlog
// gauge on every poll cycle (see runtime/outbox.Drainer and
// runtime.InstrumentedOutboxStore, which forwards the capability).
//
// Contract:
//
//   - CountPending returns the number of records currently in the PENDING
//     state for partitionKey (records already claimed by a live owner are NOT
//     pending and MUST be excluded). It never mutates state.
//   - It should be cheap relative to a Claim: back it by a COUNT / maintained
//     counter / bounded metadata read, never by materialising every record.
//     The drainer calls it at most once per poll cycle and only when there is a
//     backlog to measure, but a DynamoDB backend in particular MUST avoid a
//     full-partition scan (use a projection/counter to bound read cost).
//   - A REAL backend error (DB/read failure) is returned AS-IS. The drainer
//     treats such an error as a genuine failure — it does NOT mask it behind
//     the saturating claimed-count fallback: it SKIPS the MetricOutboxDepth
//     emission for that cycle (so a persistently broken query trips the
//     missing-data alarm) and records it via MetricOutboxDepthFailures + a
//     structured error log. A failing count never wedges the drain loop.
//   - To signal "I cannot report depth" WITHOUT it being read as a real
//     failure, return ErrOutboxDepthUnsupported (or wrap it); the drainer then
//     falls back to the claimed-count lower bound silently. This is what
//     runtime.InstrumentedOutboxStore returns when its inner store has not
//     adopted the capability.
//
// This capability is OPTIONAL: stores that do not implement it keep the
// (saturating) claimed-count fallback with no build or behaviour break.
type OutboxDepthReporter interface {
	CountPending(ctx context.Context, partitionKey string) (int, error)
}

// ErrOutboxDepthUnsupported is the sentinel an OutboxDepthReporter (or a
// wrapper forwarding the capability) returns from CountPending to mean "this
// store cannot report pending depth" — as opposed to a real backend failure.
// The drainer uses errors.Is(err, ErrOutboxDepthUnsupported) to select the
// benign saturating claimed-count FALLBACK; any OTHER non-nil error is treated
// as a genuine depth-query failure (skip the depth emission this cycle, count +
// log it) so a persistently broken count is not silently masked. Exported so
// runtime.InstrumentedOutboxStore and store adapters share one sentinel.
var ErrOutboxDepthUnsupported = errors.New("ports: outbox store does not report pending depth")

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
