package dynamodboutbox

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Store satisfies the OPTIONAL ports.OutboxDepthReporter capability so the
// drainer emits the TRUE pending backlog gauge (shared.MetricOutboxDepth)
// instead of the saturating claimed-count lower bound. The capability is
// forwarded through runtime.InstrumentedOutboxStore and probed by
// runtime/outbox.Drainer.emitDepth on every drain cycle.
var _ ports.OutboxDepthReporter = (*Store)(nil)

// CountPending reports the number of records currently in the PENDING state for
// partitionKey — the true backlog depth, independent of any claim batch size
// (ports.OutboxDepthReporter). Claimed records are NOT pending and are excluded.
//
// Cost bound (the depth contract forbids a full-partition scan for a DynamoDB
// backend): the count is served by a Select=COUNT Query against the per-partition,
// age-ordered ClaimIndex GSI (hash=PK, range=claim_sort, Projection: ALL). That
// index is SPARSE — only not-yet-terminal (pending/claimed) records carry
// claim_sort, so it holds exactly the working set for the partition and NEVER
// the completed/expired rows still awaiting TTL compaction. Select=COUNT returns
// only per-page counts (no item bodies are materialised), and the FilterExpression
// on status narrows the reported Count to the pending subset while the read cost
// stays bounded by the (pending+claimed) working set, not the whole base table.
// Per-page Count is summed across pagination so the reported depth is exact for
// the (eventually consistent) index snapshot.
//
// Degradation (returns ports.ErrOutboxDepthUnsupported — a benign "cannot report
// depth", NOT a real failure; the drainer then falls back to the saturating
// claimed-count lower bound):
//
//   - partitionKey == "" (fleet-wide, all partitions): the ClaimIndex is hashed
//     on PK, so an all-partition count would require a Scan of the whole index —
//     exactly the unbounded read the contract forbids for DynamoDB. There is no
//     status-keyed GSI to serve an O(1) global COUNT. Exact fleet depth is a
//     follow-up (add a status/counter GSI). The live drainer always calls this
//     per-partition, so this path is only reachable by an explicit fleet query.
//   - The ClaimIndex is unusable — absent on an un-migrated table, or present
//     but not Projection: ALL (so the status attribute the filter reads is not
//     projected). Without it a bounded pending COUNT is impossible without a
//     full-partition scan, so the capability is reported unavailable rather than
//     scanning. CountPending is READ-ONLY: it may READ the shared
//     claimIndexAbsent latch (set by the Claim fast path) to short-circuit, but
//     it MUST NOT WRITE it — a depth/metrics probe must never degrade the write
//     path by forcing Claim onto the exhaustive base-table scan. So this
//     degradation is per-call only; latching stays owned by Claim.
//
// A REAL backend error (a DynamoDB read failure other than an unusable index) is
// returned AS-IS (wrapped for context) so the drainer treats it as a genuine
// depth-query failure — it skips the depth emission for that cycle and records
// it via MetricOutboxDepthFailures rather than masking it behind the fallback.
func (s *Store) CountPending(ctx context.Context, partitionKey string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: count_pending", "partition_key", partitionKey)
	}

	// Fleet-wide depth has no bounded DynamoDB access path (see the doc note):
	// signal "cannot report depth" so the drainer keeps its saturating fallback.
	if partitionKey == "" {
		return 0, ports.ErrOutboxDepthUnsupported
	}

	// The only bounded pending-COUNT path is the sparse, per-partition
	// ClaimIndex. If the Claim fast path has ALREADY latched it unusable, a
	// bounded count is impossible — report unavailable rather than fall through
	// to a full-partition scan. This is a pure READ of the shared latch; see the
	// query-error branch below for why CountPending never WRITES it.
	if s.claimIndexAbsent.Load() {
		return 0, ports.ErrOutboxDepthUnsupported
	}

	names := map[string]string{"#st": "status"}
	values := map[string]ddbtypes.AttributeValue{
		":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
		":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
	}

	total := 0
	var startKey map[string]ddbtypes.AttributeValue
	for {
		// A deep working set can page more than once; honour cancellation
		// between pages so a count never outlives its context.
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			IndexName:              aws.String(claimIndexName),
			KeyConditionExpression: aws.String("PK = :pk"),
			FilterExpression:       aws.String("#st = :pending"),
			// COUNT: DynamoDB returns only the per-page (post-filter) Count; no
			// item bodies are paged back to the client, so this is a counting
			// read over the sparse working set, not a record-materialising scan.
			Select:                    ddbtypes.SelectCount,
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			if _, unusable := claimIndexUnusableReason(err); unusable {
				// Structurally unusable/absent ClaimIndex: report the capability
				// as unavailable (a benign fallback, not a real fault) for THIS
				// CALL ONLY. CountPending is a READ-ONLY depth reporter — it MUST
				// NOT call markClaimIndexUnusable / write the shared
				// claimIndexAbsent latch, or a metrics/depth probe would silently
				// force every subsequent Claim onto the exhaustive base-table
				// scan (a depth read degrading the write path). Latching stays
				// owned by the Claim fast path; CountPending only READS the latch
				// (above) to short-circuit.
				return 0, ports.ErrOutboxDepthUnsupported
			}
			return 0, wrapErr(err, "outbox count pending query failed", "partitionKey", partitionKey)
		}

		total += int(out.Count)

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	return total, nil
}
