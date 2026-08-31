package dynamodboutbox

import (
	"context"
	"errors"
	"sort"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Ordering-key and partial-claim support for the DynamoDB claim paths. Claim
// orchestration lives in acl_store.go; this file owns the two rules that make a
// per-record claim loop safe:
//
//   - the ordering-key head-of-line rule (ports.OutboxStore Claim contract): a
//     record may not be claimed while an older non-terminal sibling on the same
//     key is not itself being claimed, or the younger message is delivered
//     first and per-key order breaks with no error anywhere;
//   - a mid-batch transient failure returns the records ALREADY claimed rather
//     than discarding them. Records this owner has durably claimed are hidden
//     from CountPending and unreclaimable until the wall-clock stale window, so
//     dropping them strands real work and charges it a replay attempt per
//     recovery cycle.
//
// attrOrderingKey is the ordering key stamped by Persist, denormalised out of
// the envelope so a claim can evaluate the head-of-line rule without
// unmarshalling every scanned record. It is written only when the envelope
// carries one, so keyless records add no attribute.
const attrOrderingKey = "ordering_key"

// claimCandidate is one record considered by a claim, carrying the sort inputs
// and the ordering key so the head-of-line rule can be evaluated without
// re-reading the item.
type claimCandidate struct {
	item        map[string]ddbtypes.AttributeValue
	pk, sk      string
	createdAt   int64 // epoch millis
	seq         uint64
	envelopeID  string
	orderingKey string
	claimable   bool
}

// newClaimCandidate projects the attributes a claim decision needs out of a
// queried item.
func newClaimCandidate(item map[string]ddbtypes.AttributeValue, claimable bool) claimCandidate {
	return claimCandidate{
		item:        item,
		pk:          strAttr(item, "PK"),
		sk:          strAttr(item, "SK"),
		createdAt:   numAttrI64(item, "created_at"),
		seq:         numAttrU64(item, "seq"),
		envelopeID:  strAttr(item, "envelope_id"),
		orderingKey: orderingKeyOfItem(item),
		claimable:   claimable,
	}
}

// olderThan reports whether c sorts before other in claim order: ascending
// (created_at, seq), with the envelope ID as a final deterministic tiebreak so
// two records that somehow share a position still order the same way on every
// call.
func (c claimCandidate) olderThan(other claimCandidate) bool {
	if c.createdAt != other.createdAt {
		return c.createdAt < other.createdAt
	}
	if c.seq != other.seq {
		return c.seq < other.seq
	}
	return c.envelopeID < other.envelopeID
}

// sortOldestFirst orders candidates by ascending (created_at, seq, envelope
// id) — the ports.OutboxStore claim-ordering contract.
func sortOldestFirst(candidates []claimCandidate) {
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].olderThan(candidates[j]) })
}

// orderingKeyOfItem returns the record's ordering key, or "" when it has none.
// The attribute is the single source: Persist writes it for every keyed record,
// so a record without it has no key. There is no envelope fallback — reading
// the key back out of envelope_json would mean unmarshalling every scanned
// record on the claim path to cover data this store has never written.
func orderingKeyOfItem(item map[string]ddbtypes.AttributeValue) string {
	return strAttr(item, attrOrderingKey)
}

// isClaimableItem mirrors the claim FilterExpression client-side: a record is
// claimable when it is pending, or claimed under a strictly older fence version
// (a preempted owner), or claimed past the wall-clock stale cutoff (a
// crash-recovery reclaim). staleMs of 0 disables the wall-clock fallback, which
// is the version-only mode memory/SQLite use.
func isClaimableItem(item map[string]ddbtypes.AttributeValue, tokenVersion uint64, staleMs int64) bool {
	switch persistence.OutboxStatus(strAttr(item, "status")) {
	case persistence.OutboxPending:
		return true
	case persistence.OutboxClaimed:
		if numAttrU64(item, "claim_version") < tokenVersion {
			return true
		}
		return staleMs > 0 && numAttrI64(item, "claimed_at") < staleMs
	default:
		return false
	}
}

// blockedHeads tracks, per ordering key, the OLDEST non-terminal record a claim
// cannot take — the stranded head its younger siblings must wait behind.
//
// It deliberately keeps only the head's claim POSITION, never the DynamoDB item:
// a partition whose claimed backlog is deep and fully keyed would otherwise pin
// one full item — payload included — per key for the length of the claim. Size
// is bounded by the number of distinct blocked keys, not by the backlog.
type blockedHeads map[string]claimCandidate

// observe records c as the blocking head for its key when it is the oldest one
// seen so far. Keyless records block nothing.
func (b blockedHeads) observe(c claimCandidate) {
	if c.orderingKey == "" {
		return
	}
	c.item = nil // position only; see the type comment
	if prev, seen := b[c.orderingKey]; !seen || c.olderThan(prev) {
		b[c.orderingKey] = c
	}
}

// dropBlockedByStrandedHead removes every candidate sitting behind an older
// same-key sibling this claim cannot take — the ordering-key head-of-line rule.
//
// Candidates must already be sorted oldest-first: the filter is applied before
// truncation to `limit`, and truncation is order-preserving, so a group whose
// head falls outside the batch takes its tail with it.
func dropBlockedByStrandedHead(candidates []claimCandidate, blocked blockedHeads) []claimCandidate {
	if len(blocked) == 0 {
		return candidates
	}
	kept := candidates[:0]
	for _, c := range candidates {
		if c.orderingKey != "" {
			if head, isBlocked := blocked[c.orderingKey]; isBlocked && head.olderThan(c) {
				continue
			}
		}
		kept = append(kept, c)
	}
	return kept
}

// claimCandidates drives the per-record claim transactions for an already
// ordered, already head-of-line-filtered candidate list, APPENDING to claimed
// and returning the accumulated result. The accumulator lets the index path
// claim page by page while still treating `limit` as the batch total and
// treating everything claimed so far — including on earlier pages — as work
// that must not be discarded by a later failure.
//
// It is where the SHORT-BATCH rule lives. A per-record transaction can fail
// after earlier records were durably claimed; those records belong to this
// owner and are invisible to CountPending, so returning them alongside the
// error — which the drainer discards — strands them until the stale window and
// charges a replay attempt on every recovery cycle, eventually poisoning them
// to the dead-letter queue without a single send. So once anything is claimed,
// a transient failure ENDS the batch and returns what was claimed with a nil
// error and a truncation counter. shared.ErrStaleFencingToken is the exception:
// this owner has lost the partition and must stop and re-fence, so it is
// surfaced whatever was claimed first.
//
// A record whose own claim loses the race (claimOne returns nil, nil) is
// skipped, and — if it carries an ordering key — so is the rest of that key in
// this batch: the winner now holds the head, and taking its younger sibling
// would be exactly the overtake the head-of-line rule exists to prevent.
func (s *Store) claimCandidates(
	ctx context.Context,
	claimed []*persistence.OutboxRecord,
	candidates []claimCandidate,
	token persistence.LeaseToken,
	now time.Time,
	staleMs int64,
	limit int,
	partitionKey string,
) (out []*persistence.OutboxRecord, truncated bool, err error) {
	if claimed == nil {
		claimed = make([]*persistence.OutboxRecord, 0, min(limit, len(candidates)))
	}
	lostKeys := make(map[string]struct{})

	for i := range candidates {
		if len(claimed) >= limit {
			break
		}
		c := candidates[i]
		if c.orderingKey != "" {
			if _, lost := lostKeys[c.orderingKey]; lost {
				continue
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return s.truncatedClaim(claimed, ctxErr, partitionKey)
		}

		rec, claimErr := s.claimOne(ctx, c.item, c.pk, c.sk, token, now, staleMs)
		if claimErr != nil {
			return s.truncatedClaim(claimed, claimErr, partitionKey)
		}
		if rec == nil {
			// Lost the per-record race. Not an error, but the winner now owns
			// this key's head, so the rest of the key waits for the next cycle.
			if c.orderingKey != "" {
				lostKeys[c.orderingKey] = struct{}{}
			}
			continue
		}
		// Cache the base-table keys so Complete can address this record
		// directly instead of resolving through the lagging GSI.
		s.cacheKey(rec.ID(), c.pk, c.sk)
		claimed = append(claimed, rec)
	}
	return claimed, false, nil
}

// truncatedClaim decides what a mid-batch failure returns. Nothing claimed, or
// a lost partition (ErrStaleFencingToken): surface the error and no records.
// Otherwise the batch is legally SHORT — hand back what is durably claimed,
// count the truncation ONCE, and report truncated=true so the caller stops
// instead of issuing further reads against a store that is already failing.
func (s *Store) truncatedClaim(
	claimed []*persistence.OutboxRecord,
	err error,
	partitionKey string,
) ([]*persistence.OutboxRecord, bool, error) {
	if len(claimed) == 0 || errors.Is(err, shared.ErrStaleFencingToken) {
		return nil, true, err
	}
	s.metrics.Counter(MetricClaimTruncated, 1,
		shared.Tag{Key: shared.TagKeyPartition, Value: partitionKey})
	if s.logger != nil {
		s.logger.Warn(
			"dynamodboutbox: claim truncated by a mid-batch failure; returning the records already "+
				"claimed so they are drained instead of stranded until the stale-claim window",
			"partition_key", partitionKey,
			"claimed", len(claimed),
			"error", err.Error(),
		)
	}
	return claimed, true, nil
}
