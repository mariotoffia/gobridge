package sqliteoutbox

import (
	"strings"
	"testing"
)

// explainPlan runs EXPLAIN QUERY PLAN for query with args bound and returns the
// concatenated plan detail. Unlike queryPlan it binds parameters, so queries
// that carry a ? placeholder (countPendingByPartitionSQL) can be explained with
// a representative literal without hand-editing the production SQL.
func explainPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.sess.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}

// TestCountPendingQueryPlansAreIndexed pins CountPending bounded-cost
// contract on BOTH depth paths: the pending COUNT that backs
// ports.OutboxDepthReporter must be served by an index whose size is bounded by
// the pending backlog, never a full covering/table SCAN of every outbox row —
// for the per-partition query AND the fleet-wide (empty partitionKey) query.
//
//   - countPendingByPartitionSQL constrains partition_key, so the composite
//     idx_outbox_partition_status(partition_key, status) serves it as a bounded
//     index SEARCH.
//   - countPendingAllSQL leaves the composite index's leading column
//     unconstrained; without a status-leading index SQLite falls back to
//     "SCAN outbox USING COVERING INDEX idx_outbox_partition_status" — a full
//     scan of every row. The PARTIAL index idx_outbox_status_pending
//     (outboxIndexSQL) makes it index-served over only the pending rows.
//
// Mutation-verify: drop idx_outbox_status_pending (or repoint countPendingAllSQL
// at an unindexed column) and the all_partitions assertion fails because the
// plan reverts to the composite index / a bare table scan; restore and it
// passes.
func TestCountPendingQueryPlansAreIndexed(t *testing.T) {
	s, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, tc := range []struct {
		name string
		// query is the production SQL constant whose plan we assert.
		query string
		// args binds any ? placeholders so EXPLAIN QUERY PLAN can run.
		args []any
		// wantIndex must appear in the plan (proves the intended, bounded index
		// serves the count).
		wantIndex string
		// bannedIndex must NOT appear in the plan. For the fleet-wide path this
		// is the composite index, whose use here means a full covering scan of
		// every row (the pre-fix bounded-cost violation).
		bannedIndex string
	}{
		{
			name:      "by_partition",
			query:     countPendingByPartitionSQL,
			args:      []any{"SESSION#probe"},
			wantIndex: "idx_outbox_partition_status",
		},
		{
			name:        "all_partitions",
			query:       countPendingAllSQL,
			wantIndex:   "idx_outbox_status_pending",
			bannedIndex: "idx_outbox_partition_status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlan(t, s, tc.query, tc.args...)
			t.Logf("%s plan:\n%s", tc.name, plan)

			// The count must be index-served (bounded cost), never a bare
			// table scan.
			if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "USING COVERING INDEX") {
				t.Fatalf("%s: pending COUNT is not index-served (bounded-cost violation):\n%s", tc.name, plan)
			}

			// It must use the intended, bounded index.
			if !strings.Contains(plan, tc.wantIndex) {
				t.Fatalf("%s: pending COUNT is not served by %s (bounded-cost violation):\n%s",
					tc.name, tc.wantIndex, plan)
			}

			// It must NOT fall back to an index/scan that materialises every
			// row. For the fleet-wide path a "SCAN ... idx_outbox_partition_status"
			// is exactly the full covering scan the partial index fixes.
			if tc.bannedIndex != "" && strings.Contains(plan, tc.bannedIndex) {
				t.Fatalf("%s: pending COUNT fell back to %s — full covering scan of every row (bounded-cost violation):\n%s",
					tc.name, tc.bannedIndex, plan)
			}

			// A bare "SCAN outbox" without any index is the coarsest violation.
			if strings.Contains(plan, "SCAN outbox") && !strings.Contains(plan, "USING") {
				t.Fatalf("%s: pending COUNT does a full table scan (SCAN outbox):\n%s", tc.name, plan)
			}
		})
	}
}
