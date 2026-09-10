package ports

import (
	"context"
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// The durable outbox contract and its optional capabilities. Split out of
// stores.go because the outbox is the one store port whose behaviour cannot be
// read off its signatures: claim ordering, fencing, the ordering-key
// head-of-line rule, short batches and the depth capabilities are all contract,
// and keeping them beside the lease/DLQ/subscription ports pushed a single file
// past the size a reviewer can hold. Same package — this is a reading aid, not
// a boundary.

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
// Durability contract:
//
//   - A nil error from Persist means CRASH-DURABLE — see the crash-durable
//     success boundary in stores.go. The runtime settles the source on that
//     nil, so a store that returns nil ahead of durability turns a process
//     crash into silent loss of acknowledged work. A store that cannot meet
//     the boundary declares it through CrashDurableStoreFactory instead of
//     redefining success.
//
// Claim ordering contract:
//
//   - Claim returns records in per-partition persist order: ascending
//     (CreatedAt, Seq), where Seq is a monotonic per-partition sequence the
//     store assigns at Persist. Under a backlog deeper than `limit`, Claim
//     SELECTS the oldest-N pending records (not an arbitrary subset), because
//     per-partition send ordering depends on it.
//
//   - QueryPending returns records ordered ascending (CreatedAt, Seq) WITHIN
//     the returned set, but its SELECTION under a backlog deeper than `limit`
//     is store-defined: it is a depth/preview query (the runtime uses it only
//     to count pending records against MaxOutboxDepth), so a store MAY return
//     the first `limit` records in its native scan order rather than the
//     globally oldest-N. The in-memory and SQLite backends happen to select
//     oldest-N (ORDER BY created_at, seq LIMIT); the DynamoDB backend selects
//     in SK order for depth/preview to avoid read amplification. Callers that
//     need oldest-N SELECTION must use Claim, never QueryPending.
//
//   - ORDERING-KEY HEAD-OF-LINE RULE. A record carrying a non-empty
//     messaging.HeaderOrderingKey is claimable ONLY when the partition holds no
//     OLDER non-terminal record (pending or claimed) with the SAME ordering key
//     that this same Claim will not itself return. Ordering is a DURABLE
//     property, not a per-batch one: the drainer sequences same-key records
//     inside one claimed batch, but it cannot see a sibling left Claimed by a
//     previous cycle (a failed Release, an abandoned batch, a crashed owner).
//     Without this rule a younger sibling is claimed and delivered while the
//     stranded head still waits, and per-key order is silently violated with
//     zero errors. A blocked younger sibling is simply not returned; it becomes
//     claimable again as soon as its head reaches a terminal state or is
//     released back to pending. Records with no ordering key are unaffected and
//     keep full concurrency.
//
//     LIVENESS FOLLOWS THE HEAD. The rule delays a key's work for exactly as
//     long as its head is unreachable, so a key drains again only once its head
//     is reclaimable — via a higher fencing version, or via the wall-clock
//     stale-claim fallback where the store offers one. On a VERSION-ONLY store
//     (the in-memory backend) a head stranded at the SAME version stalls its key
//     until the version advances, exactly as that head itself is stalled. That
//     is not a regression — the head was already unreachable — but it is now
//     visible rather than silent: the blocked siblings sit in CountPending and
//     the stranded head in CountClaimed (OutboxClaimedDepthReporter), so a key
//     stalled behind a dead owner reads as standing pending depth plus standing
//     claimed depth instead of an empty backlog.
//
//   - Claim MAY return a SHORT batch (fewer than `limit` records) together with
//     a nil error when a backend claims records one at a time and a later
//     per-record write fails transiently (throttling, a deadline, a network
//     fault). Records already durably claimed at that point BELONG to the
//     caller: returning them with the error — which callers discard — would
//     strand them Claimed, hidden from CountPending, unreclaimable until the
//     wall-clock stale window, and charged a replay attempt each time. A store
//     therefore returns (claimed, nil) and counts the truncation; it returns an
//     error only when nothing was claimed, or when the failure is
//     shared.ErrStaleFencingToken (the caller has lost the partition and must
//     stop, whatever it managed to claim first).
//
//   - Claim with limit <= 0 is a fencing no-op: it validates the token and
//     advances the durable per-partition fencing high-water-mark exactly
//     like an empty-partition claim, but claims and returns no records.
//
//   - Stores MUST NOT filter claimable records by replay count. Poison
//     detection (max replay attempts) is the drainer's decision; a store
//     that hides high-replay records from Claim makes them unreachable for
//     DLQ routing.
//
// Fencing contract:
//
//   - Every guarded mutation (Claim, Expire, Complete, and
//     OutboxReleaser.Release) REQUIRES a valid fencing token: token.Owner != ""
//     AND token.Version > 0 (persistence.LeaseToken.Valid). A store MUST reject
//     a zero-value or otherwise invalid persistence.LeaseToken{} with
//     shared.ErrStaleFencingToken BEFORE performing any state transition or
//     advancing the per-partition fencing high-water-mark. A zero-value token is
//     never a real lease: accepting one would let a miswired or buggy drainer
//     claim, expire or complete work without lease ownership, defeating
//     clustering/fencing.
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
//     Claimed records are never expired here; a claimed-but-stale record
//     is reclaimed through Claim/IsClaimable, never expired out from under a
//     potentially still-valid owner.
//
//   - Expire is LEASE-FENCED against the SAME durable per-partition
//     high-water-mark as Claim, and for the same reason: it is a terminal,
//     destructive transition of records a successor could still deliver. A token
//     whose Version is strictly below the partition's high-water-mark MUST be
//     rejected with shared.ErrStaleFencingToken having expired NOTHING — a
//     preempted owner that passed a stale local lease check must not be able to
//     destroy its successor's backlog. The fence check and every record
//     transition it authorises MUST be atomic with respect to a concurrent
//     fence raise — one transaction on SQLite, the store-wide mutex on the
//     memory backend, and a per-record ConditionCheck against the fence row on
//     DynamoDB — so a takeover landing mid-sweep aborts the remainder instead
//     of racing it.
//
//   - Expire MAY return a POSITIVE count together with a non-nil error. The
//     sweep transitions records incrementally, so a run truncated by the
//     caller's deadline, or aborted part-way by a fence raise, has already made
//     those transitions durable and terminal. Callers MUST account for the
//     returned count on the error path too, or the expired term of the
//     conservation law silently under-reports.
//
//   - An ACCEPTED Expire ADVANCES the high-water-mark to token.Version, exactly
//     as Claim does, including when it expires no records. This is not
//     bookkeeping symmetry: a drop-policy drainer sweeps expiry BEFORE its
//     egress-readiness gate, so on a partition whose egress never becomes ready
//     Expire is the only fencing call that ever runs. Without the advance a
//     preempted owner could still win freshly pending work there.
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
	Expire(ctx context.Context, before time.Time, partition string, token persistence.LeaseToken) (int, error)
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

// OutboxClaimedDepthReporter is an OPTIONAL OutboxStore capability that returns
// the number of records currently CLAIMED for a partition — work an owner has
// taken but not yet driven to a terminal state.
//
// It exists because CountPending deliberately excludes claimed rows, so a
// record left Claimed by a failed Release, an abandoned batch, or a dead owner
// is invisible: the backlog gauge reads zero while messages sit undelivered.
// A claimed count separates "nothing to do" from "work is stuck", and it is
// the only signal that makes the ordering-key head-of-line rule diagnosable —
// a blocked group reports zero pending progress and a non-zero claimed depth.
//
// Contract:
//
//   - CountClaimed returns the number of records in the CLAIMED state for
//     partitionKey, regardless of which owner holds them. It never mutates.
//   - It carries the same cost expectation as CountPending: back it by a
//     COUNT / index read, never by materialising records. The drainer calls it
//     at most once per poll cycle.
//   - A REAL backend error is returned as-is; the drainer counts it via
//     MetricOutboxDepthFailures and skips the gauge for that cycle.
//   - Return ErrOutboxDepthUnsupported (or a wrap of it) to mean "I cannot
//     report claimed depth" without it being read as a failure.
//
// OPTIONAL: stores that do not implement it simply expose no claimed-depth
// gauge, with no build or behaviour break.
type OutboxClaimedDepthReporter interface {
	CountClaimed(ctx context.Context, partitionKey string) (int, error)
}
