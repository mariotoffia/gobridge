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

// Store also satisfies the OPTIONAL ports.OutboxClaimedDepthReporter capability
// so the drainer can emit the stranded-work gauge
// (shared.MetricOutboxClaimedDepth) alongside the pending backlog.
var _ ports.OutboxClaimedDepthReporter = (*Store)(nil)

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
//
// Every other failure — including a missing or under-projected ClaimIndex, which
// is a provisioning fault Preflight rejects at startup — is returned AS-IS
// (wrapped for context) so the drainer treats it as a genuine depth-query
// failure: it skips the depth emission for that cycle and records it via
// MetricOutboxDepthFailures rather than masking it behind the fallback.
func (s *Store) CountPending(ctx context.Context, partitionKey string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: count_pending", "partition_key", partitionKey)
	}
	return s.countByStatus(ctx, partitionKey, persistence.OutboxPending)
}

// CountClaimed reports the number of records currently in the CLAIMED state for
// partitionKey — work an owner took but has not driven to a terminal state
// (ports.OutboxClaimedDepthReporter). CountPending excludes those rows, so
// without this a record stranded by a failed release, an abandoned batch, or a
// dead owner is invisible: the backlog gauge reads zero while messages sit
// undelivered.
//
// It shares CountPending's cost bound and degradation rules exactly — the same
// sparse ClaimIndex Select=COUNT read, the same read-only treatment of the
// claimIndexAbsent latch, the same ports.ErrOutboxDepthUnsupported for a
// fleet-wide query or an unusable index.
func (s *Store) CountClaimed(ctx context.Context, partitionKey string) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: count_claimed", "partition_key", partitionKey)
	}
	return s.countByStatus(ctx, partitionKey, persistence.OutboxClaimed)
}

// countByStatus is the shared Select=COUNT read behind CountPending and
// CountClaimed. See CountPending's doc comment for the cost bound and the
// degradation rules; both counts obey them identically because both are served
// by the same sparse per-partition ClaimIndex.
func (s *Store) countByStatus(ctx context.Context, partitionKey string, status persistence.OutboxStatus) (int, error) {
	// Fleet-wide depth has no bounded DynamoDB access path (see the doc note):
	// signal "cannot report depth" so the drainer keeps its saturating fallback.
	if partitionKey == "" {
		return 0, ports.ErrOutboxDepthUnsupported
	}

	names := map[string]string{"#st": "status"}
	values := map[string]ddbtypes.AttributeValue{
		":pk": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
		":st": &ddbtypes.AttributeValueMemberS{Value: string(status)},
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
			FilterExpression:       aws.String("#st = :st"),
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
			return 0, wrapErr(err, "outbox count by status query failed",
				"partitionKey", partitionKey, "status", string(status))
		}

		total += int(out.Count)

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	return total, nil
}
