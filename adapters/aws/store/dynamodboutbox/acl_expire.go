package dynamodboutbox

// Bulk expiry of the outbox: the lease-fenced sweep that transitions pending
// records past their expires_at to the terminal expired state, its per-record
// transaction, and the resume cursor that keeps a deadline-truncated sweep
// making forward progress. Split out of acl_store.go to keep that file from
// growing further past the repository's file-length limit.

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Expire marks pending records whose ExpiresAt is before the given time as
// expired, SCOPED to the supplied partition. Claimed records are never
// expired here, and records in other partitions are left untouched even when
// past their expiry. Returns the count.
//
// Candidates come from the sparse ExpiryIndex (only records persisted with
// a non-zero ExpiresAt are in it); the index is hashed on the has_expiry flag
// (not the partition), so the query is scoped to the partition with a
// FilterExpression on the projected PK.
// A sweep truncated by the caller's deadline resumes at the page it stopped on
// (see expireByStatus). The per-record conditional update gates
// on pending status, so claimed/terminal candidates are skipped without error.
// The sweep is lease-fenced exactly as Claim is: the partition fence is read
// and raised up front, and EVERY per-record transition additionally
// condition-checks the fence row inside a transaction, so a takeover landing
// mid-sweep aborts the remainder rather than racing it.
func (s *Store) Expire(ctx context.Context, before time.Time, partition string, token persistence.LeaseToken) (int, error) {
	// Expiry terminally destroys pending records a successor could still
	// deliver, so it carries the same valid-token requirement as Claim and
	// Complete: a zero-value token is never a real lease.
	if !token.Valid() {
		return 0, shared.ErrStaleFencingToken.
			WithMessage("expire rejected: invalid (zero-value) fencing token").
			With("givenOwner", token.Owner).
			With("givenVersion", token.Version)
	}

	// Version-monotonic fence, same high-water-mark Claim enforces: a preempted
	// owner cannot expire work its successor is entitled to drain.
	maxVersion, err := s.maxClaimVersion(ctx, partition)
	if err != nil {
		return 0, err
	}
	if token.Version < maxVersion {
		return 0, shared.ErrStaleFencingToken.
			WithMessage("expire rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", maxVersion)
	}
	// Raise the fence to this accepted version. A drop-policy drainer sweeps
	// expiry before its egress-readiness gate, so on a partition whose egress
	// never becomes ready this is the only fence advance that ever runs.
	if err := s.raiseFence(ctx, partition, token.Version, s.clk.Now()); err != nil {
		return 0, err
	}

	beforeMs := before.UnixMilli()
	ttlEpoch := before.Add(s.compactGrace).Unix()
	// Pending-only: a claimed record is reclaimed via Claim/IsClaimable,
	// never expired out from under a potentially still-valid owner.
	return s.expireByStatus(ctx, string(persistence.OutboxPending), partition, beforeMs, ttlEpoch, token)
}

// expireByStatus sweeps one partition's expiry-eligible records through the
// sparse ExpiryIndex, whose range key is expires_at — so the read is bounded to
// records actually DUE, not to the partition's whole pending backlog.
//
// The index is hashed on the single has_expiry flag rather than on PK, so the
// scan crosses every partition and this one is selected with a FilterExpression.
// Rows in partitions this drainer never sweeps are never removed from the index,
// so they accumulate at the head of the range permanently. Once the caller
// bounds the sweep with an operation deadline, restarting at page 1 every time
// would let a large enough foreign backlog consume the whole budget before this
// partition's records are reached — expiry would stall forever. So a truncated
// sweep REMEMBERS the page it stopped on and resumes there next time, making
// progress monotonic regardless of how much foreign traffic sits ahead of it.
//
// A cursor is kept ONLY when the sweep that set it made progress past where it
// started. That is what keeps the resume from becoming its own trap: a sweep
// that fails on the very page it resumed from — a persistently throttled store,
// a flapping fence, a clock that stepped backwards past the cursor's key —
// advances nothing, so its cursor is dropped and the next sweep starts at the
// head. Without that rule a repeatedly-failing partition would pin the cursor
// forever and never look at the records ahead of it again.
//
// The cursor is also cleared when a pass completes, so the next pass starts from
// the beginning and picks up records that became due behind it. A record that
// becomes due behind an in-flight cursor therefore waits at most until the
// current pass finishes or stalls.
//
// ponytail: the durable fix is an index hashed on PK and sparse on expiry, which
// needs a table migration; the cursor buys the bound without one.
func (s *Store) expireByStatus(
	ctx context.Context,
	status, partition string,
	beforeMs, ttlEpoch int64,
	token persistence.LeaseToken,
) (int, error) {
	count := 0
	resumedFrom := s.loadExpiryCursor(partition)
	startKey := resumedFrom
	// advanced reports whether this sweep consumed at least one page beyond the
	// point it resumed from; see the cursor rule above.
	advanced := false

	for {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			IndexName:              aws.String(expiryIndexName),
			KeyConditionExpression: aws.String("#he = :flag AND expires_at < :before"),
			// FilterExpression scopes the index scan (hashed on has_expiry, not
			// the partition) to a single partition so a lease-holder's sweep
			// never expires another partition's records.
			FilterExpression: aws.String("#pk = :partition"),
			ExpressionAttributeNames: map[string]string{
				"#he": attrHasExpiry,
				"#pk": "PK",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":flag":      &ddbtypes.AttributeValueMemberS{Value: hasExpiryFlag},
				":before":    &ddbtypes.AttributeValueMemberN{Value: i64(beforeMs)},
				":partition": &ddbtypes.AttributeValueMemberS{Value: partition},
			},
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			// Resume at the page that failed rather than re-reading everything
			// ahead of it — but only if this sweep got further than where it
			// started, otherwise the resume point is exactly what is failing.
			s.parkExpiryCursor(partition, startKey, advanced)
			return count, wrapErr(err, "outbox expire query failed", "status", status)
		}

		expired, expireErr := s.expireItems(ctx, out.Items, status, beforeMs, ttlEpoch, token)
		count += expired
		if expireErr != nil {
			s.parkExpiryCursor(partition, startKey, advanced)
			return count, expireErr
		}

		if out.LastEvaluatedKey == nil {
			// Pass complete: the next sweep starts from the head again.
			s.clearExpiryCursor(partition)
			return count, nil
		}
		startKey = out.LastEvaluatedKey
		advanced = true
	}
}

// loadExpiryCursor returns the page a previous truncated sweep of this partition
// stopped on, or nil to start from the head of the index.
func (s *Store) loadExpiryCursor(partition string) map[string]ddbtypes.AttributeValue {
	s.expiryCursorMu.Lock()
	defer s.expiryCursorMu.Unlock()
	return s.expiryCursors[partition]
}

// maxExpiryCursors bounds the resume-cursor map. A cursor is only ever an
// optimisation — losing one costs a pass that restarts at the head of the index,
// never correctness — so when a process sweeps more partitions than this (a
// long-lived host cycling through ephemeral sessions, whose finished partitions
// would otherwise leave their entries behind forever) the map is simply reset
// rather than grown or LRU-tracked.
const maxExpiryCursors = 1024

// parkExpiryCursor stores the resume point of an interrupted sweep, or drops any
// existing one when the sweep never got past where it started — a resume point
// that is itself the thing failing would otherwise be retried forever while the
// records ahead of it are never examined again.
func (s *Store) parkExpiryCursor(partition string, key map[string]ddbtypes.AttributeValue, advanced bool) {
	if !advanced {
		s.clearExpiryCursor(partition)
		return
	}
	s.saveExpiryCursor(partition, key)
}

// saveExpiryCursor records where an interrupted sweep should resume. A nil key
// means the sweep was interrupted on the first page, which is already the
// default starting point, so nothing needs remembering.
func (s *Store) saveExpiryCursor(partition string, key map[string]ddbtypes.AttributeValue) {
	if key == nil {
		return
	}
	s.expiryCursorMu.Lock()
	defer s.expiryCursorMu.Unlock()
	if s.expiryCursors == nil {
		s.expiryCursors = make(map[string]map[string]ddbtypes.AttributeValue)
	}
	if len(s.expiryCursors) >= maxExpiryCursors {
		if _, resuming := s.expiryCursors[partition]; !resuming {
			s.expiryCursors = make(map[string]map[string]ddbtypes.AttributeValue, 1)
		}
	}
	s.expiryCursors[partition] = key
}

// clearExpiryCursor drops the resume point once a pass completes. Cursors exist
// only for partitions with an interrupted sweep in flight, so the map holds at
// most one entry per such partition and shrinks as they finish.
func (s *Store) clearExpiryCursor(partition string) {
	s.expiryCursorMu.Lock()
	defer s.expiryCursorMu.Unlock()
	delete(s.expiryCursors, partition)
}

// expireItems transitions one page of candidates, returning how many it
// actually expired. It stops at the first error so a fence raise mid-page
// aborts the sweep, and reports the count achieved before that point — those
// transitions are already durable.
func (s *Store) expireItems(
	ctx context.Context,
	items []map[string]ddbtypes.AttributeValue,
	status string,
	beforeMs, ttlEpoch int64,
	token persistence.LeaseToken,
) (int, error) {
	count := 0
	for _, item := range items {
		expired, err := s.expireOne(ctx, strAttr(item, "PK"), strAttr(item, "SK"), status, beforeMs, ttlEpoch, token)
		if err != nil {
			return count, err
		}
		if expired {
			count++
		}
	}
	return count, nil
}

// expireOne transitions a single candidate record to expired inside a
// TransactWriteItems that also condition-checks the partition FENCE row, so a
// concurrently raised fence (a higher-version owner took the partition mid-sweep)
// aborts the expiry instead of destroying the successor's backlog. It mirrors
// claimOne; expiry is terminal, so it earns the same protection a claim gets.
//
// Returns (false, nil) when only the RECORD-level condition failed — the
// candidate was claimed or completed between the index read and this write, so
// it is skipped, not an error. Returns shared.ErrStaleFencingToken when the
// FENCE check failed, which aborts the whole sweep.
func (s *Store) expireOne(
	ctx context.Context,
	pk, sk, status string,
	beforeMs, ttlEpoch int64,
	token persistence.LeaseToken,
) (bool, error) {
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []ddbtypes.TransactWriteItem{
			{
				ConditionCheck: &ddbtypes.ConditionCheck{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
					},
					ConditionExpression:      aws.String("attribute_not_exists(#mcv) OR #mcv <= :ver"),
					ExpressionAttributeNames: map[string]string{"#mcv": attrMaxClaimVersion},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":ver": &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
					},
				},
			},
			{
				Update: &ddbtypes.Update{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
					},
					// REMOVE has_expiry drops the now-terminal record out of the
					// sparse ExpiryIndex so later Expire passes never re-scan it;
					// REMOVE claim_sort drops it out of the sparse ClaimIndex.
					UpdateExpression: aws.String("SET #st = :expired, #ttl = :ttl REMOVE " + attrHasExpiry + ", " + attrClaimSort),
					// Condition gates on the status this pass is allowed to
					// expire (pending-only from Expire) plus the expiry window,
					// so a candidate that was claimed or completed between the
					// index read and this write is skipped, not corrupted.
					ConditionExpression: aws.String(
						"#st = :status AND expires_at > :zero AND expires_at < :before"),
					ExpressionAttributeNames: map[string]string{
						"#st":  "status",
						"#ttl": "ttl",
					},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":expired": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxExpired)},
						":status":  &ddbtypes.AttributeValueMemberS{Value: status},
						":zero":    &ddbtypes.AttributeValueMemberN{Value: "0"},
						":before":  &ddbtypes.AttributeValueMemberN{Value: i64(beforeMs)},
						":ttl":     &ddbtypes.AttributeValueMemberN{Value: i64(ttlEpoch)},
					},
				},
			},
		},
	})
	if err == nil {
		return true, nil
	}

	reasons, canceled := transactCancellationCodes(err)
	if !canceled {
		return false, wrapErr(err, "outbox expire update failed", "partitionKey", pk)
	}
	// A cancellation with NO per-item reason codes is not a recognisable lost
	// race. It must not fall through to the benign skip below — that would
	// silently leave the record un-expired while the sweep reported success.
	if len(reasons) == 0 {
		return false, wrapErr(err, "outbox expire transaction canceled with no reasons",
			"partitionKey", pk, "ownerID", token.Owner)
	}
	// Item 0 is the fence check, evaluated first: its failure means a
	// higher-version owner took the partition between our fence raise and this
	// write. Stop the sweep — every remaining candidate now belongs to the
	// successor, and there is nothing to retry under this stale token.
	if reasons[0] == ccReasonCondCheckFailed {
		return false, shared.ErrStaleFencingToken.
			WithMessage("expire aborted: partition fence advanced past token version").
			With("givenVersion", token.Version).
			With("partitionKey", pk)
	}
	// A reason outside the benign contention set (throttling, validation, ...)
	// is a real fault: surface it so the drainer backs off instead of treating a
	// throttled partition as "nothing to expire".
	if code, faulted := nonContentionCancellation(reasons); faulted {
		return false, wrapErr(err, "outbox expire transaction canceled",
			"partitionKey", pk, "ownerID", token.Owner, "cancellationReason", code)
	}
	// Record-level condition failure or a transient transaction conflict: the
	// candidate changed state under us. Skip it.
	return false, nil
}
