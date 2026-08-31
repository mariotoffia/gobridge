package sqliteoutbox

import (
	"fmt"
	"strings"
)

// SQL strings and dynamic-clause builders for the outbox table.
//
// This file is the SQL-construction half of the SQLite ACL: it owns
// every query string the adapter sends to the driver. Row scanning
// lives in acl_row.go and lifecycle/exec wiring lives in
// acl_session.go.

// outboxColumnDefs is the outbox table's column-definition block.
//
// It carries NO inline UNIQUE(envelope_id, binding_id). A GLOBAL constraint
// would diverge from the ports.OutboxStore Persist identity — (partition key,
// EnvelopeID, BindingID) — and silently swallow a re-persist of the same
// envelope+binding under a NEW partition key as a duplicate (a silent-loss edge
// on a session-identity change; the DynamoDB backend delivers it). Duplicate
// detection is the partition-scoped UNIQUE INDEX idx_outbox_identity (see
// outboxIndexSQL), matching the DynamoDB backend and the contract.
const outboxColumnDefs = `
    id             TEXT PRIMARY KEY,
    partition_key  TEXT NOT NULL,
    route_id       TEXT NOT NULL,
    envelope_id    TEXT NOT NULL,
    binding_id     TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    address        TEXT NOT NULL DEFAULT '',
    envelope_json  TEXT NOT NULL,
    headers_json   TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    claimed_by     TEXT NOT NULL DEFAULT '',
    claim_version  INTEGER NOT NULL DEFAULT 0,
    claimed_at     INTEGER NOT NULL DEFAULT 0,
    first_attempted_at INTEGER NOT NULL DEFAULT 0,
    replay_count   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    completed_at   INTEGER NOT NULL DEFAULT 0,
    seq            INTEGER NOT NULL DEFAULT 0,
    ordering_key   TEXT NOT NULL DEFAULT ''`

// schemaSQL is the DDL run on every NewStore. Idempotent: it creates the base
// tables only, and outboxIndexSQL creates every index immediately after.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS outbox (` + outboxColumnDefs + `
);
CREATE TABLE IF NOT EXISTS outbox_partition_fence (
    partition_key TEXT PRIMARY KEY,
    max_version   INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);
`

// outboxIndexSQL creates every index on the outbox table. Run on every open
// immediately after schemaSQL, and idempotent:
//
//   - idx_outbox_partition_status backs the Claim/QueryPending partition scans.
//     It also serves CountPending's per-partition path (countPendingByPartitionSQL,
//     which constrains partition_key so the composite index applies).
//   - idx_outbox_status_pending is a PARTIAL index on status keeping only the
//     'pending' rows. It bounds CountPending's FLEET-WIDE path
//     (countPendingAllSQL, WHERE status = 'pending' with NO partition_key): the
//     composite idx_outbox_partition_status cannot serve that query (its leading
//     column partition_key is unconstrained, so the planner falls back to a full
//     covering scan of every row), whereas this tiny status-only partial index
//     lets the fleet-wide COUNT be served by an index whose size is exactly the
//     pending backlog — bounded cost, and near-free to maintain since it holds
//     only pending rows (the CountPending bounded-cost contract).
//   - idx_outbox_identity is the partition-scoped duplicate-detection identity
//     (partition_key, envelope_id, binding_id) that INSERT OR IGNORE keys on.
//     Partition-scoped, not global, so the same envelope+binding re-persisted
//     under a new partition is a distinct claimable record (contract parity
//     with DynamoDB).
//   - idx_outbox_completed / idx_outbox_expired are PARTIAL indexes that turn
//     the retention-compaction DELETEs (deleteCompletedSQL / deleteExpiredSQL)
//     from full-table scans into narrow index range scans.
//   - idx_outbox_ordering is a PARTIAL index over the ordering-keyed rows only.
//     It backs the head-of-line subquery in selectClaimableIDsSQL, which asks
//     "does this key have an older non-terminal sibling I am not taking?" once
//     per candidate. Without it that subquery degrades to a partition scan per
//     candidate; with it, it is a narrow range seek. It stays small because
//     most deployments key only a subset of their traffic.
const outboxIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
CREATE INDEX IF NOT EXISTS idx_outbox_status_pending ON outbox(status) WHERE status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_identity ON outbox(partition_key, envelope_id, binding_id);
CREATE INDEX IF NOT EXISTS idx_outbox_completed ON outbox(completed_at) WHERE status = 'completed';
CREATE INDEX IF NOT EXISTS idx_outbox_expired ON outbox(expires_at) WHERE status = 'expired';
CREATE INDEX IF NOT EXISTS idx_outbox_ordering ON outbox(partition_key, ordering_key, created_at, seq) WHERE ordering_key <> '';
`

// outboxColumns is the canonical column list used for SELECTs that
// hydrate full persistence.OutboxRecord values via scanRecords.
// first_attempted_at is appended last so scanOutboxRecords can bind it
// at the tail without shifting any existing scan positions.
const outboxColumns = `id, partition_key, route_id, envelope_id, binding_id, session_id,
        address, envelope_json, headers_json, status, claimed_by, claim_version,
        claimed_at, replay_count, created_at, expires_at, completed_at, seq, first_attempted_at`

const (
	// insertOutboxSQL persists one record with per-record idempotency
	// (ports.OutboxStore Persist contract): OR IGNORE skips a row whose
	// identity (id primary key or the partition-scoped idx_outbox_identity
	// unique index — partition_key, envelope_id, binding_id) already
	// exists, letting the caller count RowsAffected to detect an
	// all-duplicate batch. seq is the monotonic per-partition persist
	// sequence: MAX(seq)+1 within the partition is race-free here because
	// the pool is capped at a single writer connection and the batch
	// runs in one transaction.
	//
	// first_attempted_at is intentionally omitted from the column list, so a
	// freshly persisted row always starts at DEFAULT 0 and the store-side claim
	// CASE-WHEN (updateClaimSQL) is the single authority that stamps it. This is
	// safe because records are only ever Persisted with a zero first attempt:
	// they are created via NewOutboxRecord, and re-Persist is INSERT OR IGNORE
	// (no overwrite, so a re-Persisted row cannot backfill the column either).
	// A future author who rehydrates then re-Persists an already-stamped record
	// must add first_attempted_at here AND to the bind order, or the budget
	// clock would silently reset. The memory and DynamoDB stores preserve a
	// non-zero FirstAttemptedAt through Persist; SQLite relies on this invariant.
	//
	// ordering_key is denormalised out of the envelope so Claim's head-of-line
	// subquery is an indexed range seek instead of a JSON extraction per row.
	// It is written once at Persist and never rewritten: the envelope is
	// immutable, so the key cannot change under a record.
	//
	// Bind order: ..., created_at, expires_at, ordering_key,
	// partition_key (again, for the seq subselect).
	insertOutboxSQL = `INSERT OR IGNORE INTO outbox (id, partition_key, route_id, envelope_id, binding_id,
		 session_id, address, envelope_json, headers_json, status, created_at, expires_at, ordering_key, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?,
		 (SELECT COALESCE(MAX(seq), 0) + 1 FROM outbox WHERE partition_key = ?))`

	// selectMaxClaimVersionSQL reports the highest claim_version ever stamped on
	// a persisted row in the partition. It is the recovery component of the
	// fence: retention compaction may drop the fence entry of a partition that
	// still holds claimed records, and their stamped claim_version keeps the
	// high-water-mark from resetting. The durable fence (selectFenceVersionSQL)
	// is the authoritative source.
	selectMaxClaimVersionSQL = `SELECT COALESCE(MAX(claim_version), 0)
		 FROM outbox WHERE partition_key = ?`

	// selectFenceVersionSQL reads the durable per-partition fencing
	// high-water-mark: the highest token.Version observed on ANY Claim,
	// including a no-op Claim that returned no rows. This is what makes the
	// SQLite fence match the memory backend's latestVersion semantics.
	selectFenceVersionSQL = `SELECT COALESCE(MAX(max_version), 0)
		 FROM outbox_partition_fence WHERE partition_key = ?`

	// upsertFenceVersionSQL advances the durable high-water-mark on every
	// Claim. MAX(...) keeps it monotonic so an out-of-order older token can
	// never lower the fence. updated_at is refreshed on every touch so
	// retention compaction can eventually drop fences of abandoned
	// (ephemeral/rotating) partitions instead of growing the table forever.
	// Bind order: partition_key, max_version, updated_at_ms.
	upsertFenceVersionSQL = `INSERT INTO outbox_partition_fence (partition_key, max_version, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(partition_key) DO UPDATE SET
		 max_version = MAX(max_version, excluded.max_version),
		 updated_at = excluded.updated_at`

	selectPendingByPartitionSQL = `SELECT ` + outboxColumns + `
		 FROM outbox
		 WHERE partition_key = ? AND status = 'pending'
		 ORDER BY created_at, seq
		 LIMIT ?`

	// countPendingByPartitionSQL / countPendingAllSQL back the OPTIONAL
	// ports.OutboxDepthReporter capability: an efficient COUNT of pending rows
	// (the true backlog behind shared.MetricOutboxDepth) served by an index
	// rather than materialising every row like selectPendingByPartitionSQL.
	//   - countPendingByPartitionSQL constrains partition_key, so the composite
	//     idx_outbox_partition_status(partition_key, status) serves it as a
	//     bounded index SEARCH.
	//   - countPendingAllSQL counts across every partition (empty partition key).
	//     Its leading composite-index column partition_key is unconstrained, so
	//     it is served by the status-leading PARTIAL index idx_outbox_status_pending
	//     (see outboxIndexSQL) whose size is exactly the pending backlog — never a
	//     full covering scan of every row. TestCountPendingQueryPlansAreIndexed
	//     pins this bounded-cost plan for both paths.
	countPendingByPartitionSQL = `SELECT COUNT(*) FROM outbox
		 WHERE partition_key = ? AND status = 'pending'`
	countPendingAllSQL = `SELECT COUNT(*) FROM outbox WHERE status = 'pending'`

	// countClaimedByPartitionSQL / countClaimedAllSQL back the OPTIONAL
	// ports.OutboxClaimedDepthReporter capability: the count of records an owner
	// has taken but not yet driven to a terminal state. CountPending excludes
	// those rows, so without this a record stranded by a failed release reads as
	// an empty backlog. The per-partition path is served by the composite
	// idx_outbox_partition_status; the fleet-wide path has no partition
	// constraint and scans the status index.
	countClaimedByPartitionSQL = `SELECT COUNT(*) FROM outbox
		 WHERE partition_key = ? AND status = 'claimed'`
	countClaimedAllSQL = `SELECT COUNT(*) FROM outbox WHERE status = 'claimed'`

	expireOutboxSQL = `UPDATE outbox SET status = 'expired'
		 WHERE partition_key = ? AND expires_at > 0 AND expires_at < ? AND status = 'pending'`

	// Retention compaction (mirrors the DynamoDB backend's TTL grace):
	// terminal rows — completed or expired — older than the retention window
	// are physically deleted so the single-file production backend does not
	// grow unboundedly. The idx_outbox_completed / idx_outbox_expired partial
	// indexes back these DELETEs so a sweep is an index range scan, not a
	// full-table scan. Deleting a terminal row also releases its
	// (partition_key, envelope_id, binding_id) dedup identity, so the
	// retention window must comfortably exceed any upstream redelivery window
	// (same tradeoff as DynamoDB item TTL).
	deleteCompletedSQL = `DELETE FROM outbox
		 WHERE status = 'completed' AND completed_at > 0 AND completed_at < ?`
	deleteExpiredSQL = `DELETE FROM outbox
		 WHERE status = 'expired' AND expires_at > 0 AND expires_at < ?`

	// deleteStaleFencesSQL drops fence rows untouched for longer than the
	// fence retention window (max(retention, 30d)). Losing a fence after a
	// month of abandonment is acceptable: a partition idle that long has no
	// competing owners left to fence, and per-partition seq restarting at 1
	// cannot reorder claims because created_at is the primary sort key.
	deleteStaleFencesSQL = `DELETE FROM outbox_partition_fence
		 WHERE updated_at > 0 AND updated_at < ?`
)

// claimableWhere is the reusable predicate identifying rows a Claim may take:
// a pending row, a row claimed under a strictly-older fence version (a
// preempted owner), or — when time-stale reclaim is enabled — a row that is
// still claimed but whose claim was stranded past the stale cutoff (
// crash-recovery for an owner that died mid-drain without completing or
// releasing, mirroring the DynamoDB backend's stale-claim fallback). The
// claim_version placeholder is always present; the claimed_at cutoff
// placeholder is present only when staleEnabled, so a store configured
// without a stale-claim duration stays strictly version-only.
func claimableWhere(staleEnabled bool) string {
	return claimableWhereFor("", staleEnabled)
}

// claimableWhereFor is claimableWhere qualified with a table alias, so the same
// predicate can be applied to the candidate row and, inside the head-of-line
// subquery, to a sibling row in the same statement. Pass "" for an unqualified
// single-table statement.
func claimableWhereFor(alias string, staleEnabled bool) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	w := `(` + q + `status = 'pending' OR (` + q + `status = 'claimed' AND ` + q + `claim_version < ?)`
	if staleEnabled {
		w += ` OR (` + q + `status = 'claimed' AND ` + q + `claimed_at > 0 AND ` + q + `claimed_at <= ?)`
	}
	return w + `)`
}

// headOfLineWhere is the ordering-key head-of-line predicate (see the
// ports.OutboxStore Claim contract). A candidate carrying an ordering key is
// claimable only when no OLDER non-terminal row shares that key without being
// claimable itself — a head stranded Claimed by a failed release, an abandoned
// batch, or a dead owner. Claiming past such a head would deliver a younger
// message first and silently break per-key order, with no error anywhere.
//
// The subquery repeats claimableWhere so a blocker that THIS claim will also
// take does not block: the ORDER BY returns the head first, so the whole group
// travels together and in order. Truncating at LIMIT is order-preserving, so a
// group whose head falls outside the batch takes its tail with it.
//
// The claim_version placeholder (and the stale cutoff, when enabled) therefore
// appears twice: once for the candidate, once for the sibling test.
func headOfLineWhere(staleEnabled bool) string {
	return `(o.ordering_key = '' OR NOT EXISTS (
		 SELECT 1 FROM outbox b
		 WHERE b.partition_key = o.partition_key
		   AND b.ordering_key = o.ordering_key
		   AND b.status IN ('pending', 'claimed')
		   AND (b.created_at < o.created_at
		        OR (b.created_at = o.created_at AND b.seq < o.seq))
		   AND NOT ` + claimableWhereFor("b", staleEnabled) + `
	 ))`
}

// selectClaimableIDsSQL builds the claimable-ID SELECT. Bind order:
// partition_key, claim_version, [stale_cutoff_ms], claim_version,
// [stale_cutoff_ms], limit.
func selectClaimableIDsSQL(staleEnabled bool) string {
	return `SELECT o.id FROM outbox o
		 WHERE o.partition_key = ? AND ` + claimableWhereFor("o", staleEnabled) + `
		 AND ` + headOfLineWhere(staleEnabled) + `
		 ORDER BY o.created_at, o.seq
		 LIMIT ?`
}

// updateClaimSQL builds the UPDATE statement that flips n records
// from pending/claimed to claimed under a new owner+version and stamps
// claimed_at so a later stale-claim reclaim can find a stranded row. It
// also stamps first_attempted_at with a CASE-WHEN "stamp once" write: the
// replay-budget clock is set the FIRST time a row is claimed (first_attempted_at
// still 0) and never moved by a later reclaim. The SQLite store claims via SQL
// without rehydrating and calling OutboxRecord.Claim, so this CASE mirrors the
// aggregate's stamp-once guard on the store side. The WHERE clause repeats the
// claimableWhere fence (the claim UPDATE is guarded, not a blind id-list
// write) so a row that stopped being claimable between select and update is
// never stolen.
//
// Bind order:
// claimed_by, claim_version, claimed_at, first_attempted_at, ids..., claim_version, [stale_cutoff_ms].
func updateClaimSQL(n int, staleEnabled bool) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'claimed', claimed_by = ?, claim_version = ?,
			 claimed_at = ?,
			 first_attempted_at = CASE WHEN first_attempted_at = 0 THEN ? ELSE first_attempted_at END,
			 replay_count = replay_count + 1
			 WHERE id IN (%s) AND `+claimableWhere(staleEnabled),
		placeholders(n),
	)
}

// selectByIDsSQL builds the SELECT that hydrates n records by ID.
func selectByIDsSQL(n int) string {
	return fmt.Sprintf(
		`SELECT %s FROM outbox WHERE id IN (%s) ORDER BY created_at, seq`,
		outboxColumns, placeholders(n),
	)
}

// updateCompleteSQL builds the UPDATE statement that marks n records
// completed iff they are still claimed by the token owner at the
// supplied claim_version (owner+version+status fence).
func updateCompleteSQL(n int) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'completed', completed_at = ?
			 WHERE id IN (%s) AND status = 'claimed' AND claimed_by = ? AND claim_version = ?`,
		placeholders(n),
	)
}

// releaseOutboxSQL builds the UPDATE statement that returns n records
// from claimed back to pending iff they are still claimed by the token
// owner at the supplied claim_version (owner+version+status fence, the
// same fence as updateCompleteSQL). It clears claimed_by and claimed_at
// so the row is re-claimable on the next drain and hydrates identically
// to the domain aggregate after Release (which zeroes both);
// claim_version is left as-is (it is irrelevant once pending and is
// overwritten by the next Claim). replay_count is untouched — the next
// Claim increments it, preserving the poison-message cap.
func releaseOutboxSQL(n int) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'pending', claimed_by = '', claimed_at = 0
			 WHERE id IN (%s) AND status = 'claimed' AND claimed_by = ? AND claim_version = ?`,
		placeholders(n),
	)
}

// placeholders returns "?,?,?" repeated n times.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}
