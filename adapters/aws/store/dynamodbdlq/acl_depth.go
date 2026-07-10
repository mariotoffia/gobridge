package dynamodbdlq

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Store satisfies the OPTIONAL ports.DLQDepthReporter capability so the runtime
// can sample shared.MetricDLQDepth (the standing DLQ backlog) via
// runtime.ReportDLQDepth. Without it the DLQ-depth gauge is simply not emitted.
var _ ports.DLQDepthReporter = (*Store)(nil)

// Depth reports the number of outstanding DLQ entries currently stored across
// all routes/categories (ports.DLQDepthReporter). Every stored item IS an
// outstanding entry — entries leave the table only via Delete/Purge/TTL — so the
// backlog equals the table item count.
//
// Cost bound (the depth contract requires "an efficient COUNT / item-count
// metadata read, not paging every entry"): the DLQ table is keyed only on PK
// (one item per entry) with GSIs sparse on route_id/category — there is NO
// status-partitioned index that could serve a bounded Select=COUNT of ALL
// entries, and a full-table Scan is exactly the per-entry paging the contract
// forbids. Depth therefore reads DynamoDB's maintained item-count metadata via a
// single DescribeTable call: an O(1), no-scan read that is the DynamoDB-idiomatic
// "item-count metadata read".
//
// Accuracy trade-off (KNOWN LIMITATION — reported as a follow-up): DynamoDB
// refreshes TableDescription.ItemCount only approximately every six hours, so
// recent writes/deletes may not be reflected. For a periodically-sampled backlog
// gauge this bounded staleness is the accepted degradation. Exact, real-time DLQ
// depth needs a maintained counter item (incremented on Write, decremented on
// Delete/Purge inside the same conditional write) — a cross-cutting write-path
// change deliberately out of scope here.
//
// A transient backend error is returned AS-IS (wrapped for context); the caller
// (runtime.ReportDLQDepth) treats it as "depth unavailable this sample" and emits
// nothing. Never a full scan, never swallowed.
func (s *Store) Depth(ctx context.Context) (int, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodbdlq: depth")
	}

	out, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return 0, wrapErr(err, "dlq depth describe-table failed", "table", s.tableName)
	}
	// A freshly created table (or DynamoDB Local before its first metadata
	// refresh) can report a nil/zero ItemCount; treat a missing count as an
	// empty backlog rather than an error.
	if out.Table == nil || out.Table.ItemCount == nil {
		return 0, nil
	}

	return int(*out.Table.ItemCount), nil
}
