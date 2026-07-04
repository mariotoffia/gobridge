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

// schemaSQL is the DDL run on every NewStore. Idempotent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS outbox (
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
    UNIQUE(envelope_id, binding_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
CREATE TABLE IF NOT EXISTS outbox_partition_fence (
    partition_key TEXT PRIMARY KEY,
    max_version   INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);
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
	// identity (id primary key or UNIQUE(envelope_id, binding_id)) already
	// exists, letting the caller count RowsAffected to detect an
	// all-duplicate batch. seq is the monotonic per-partition persist
	// sequence: MAX(seq)+1 within the partition is race-free here because
	// the pool is capped at a single writer connection (I2) and the batch
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
	// Bind order: ..., created_at, expires_at,
	// partition_key (again, for the seq subselect).
	insertOutboxSQL = `INSERT OR IGNORE INTO outbox (id, partition_key, route_id, envelope_id, binding_id,
		 session_id, address, envelope_json, headers_json, status, created_at, expires_at, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?,
		 (SELECT COALESCE(MAX(seq), 0) + 1 FROM outbox WHERE partition_key = ?))`

	// selectMaxClaimVersionSQL reports the highest claim_version ever stamped
	// on a persisted row in the partition. It is only the legacy-row component
	// of the fence: rows predating outbox_partition_fence have no fence entry,
	// so their stamped claim_version is still honoured. The durable fence
	// (selectFenceVersionSQL) is the authoritative high-water-mark.
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

	expireOutboxSQL = `UPDATE outbox SET status = 'expired'
		 WHERE expires_at > 0 AND expires_at < ? AND status = 'pending'`

	// Retention compaction (mirrors the DynamoDB backend's TTL grace):
	// terminal rows — completed or expired — older than the retention window
	// are physically deleted so the single-file production backend does not
	// grow unboundedly. Deleting a terminal row also releases its
	// UNIQUE(envelope_id, binding_id) dedup identity, so the retention window
	// must comfortably exceed any upstream redelivery window (same tradeoff
	// as DynamoDB item TTL).
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
// still claimed but whose claim was stranded past the stale cutoff (I1:
// crash-recovery for an owner that died mid-drain without completing or
// releasing, mirroring the DynamoDB backend's stale-claim fallback). The
// claim_version placeholder is always present; the claimed_at cutoff
// placeholder is present only when staleEnabled, so a store configured
// without a stale-claim duration stays strictly version-only.
func claimableWhere(staleEnabled bool) string {
	w := `(status = 'pending' OR (status = 'claimed' AND claim_version < ?)`
	if staleEnabled {
		w += ` OR (status = 'claimed' AND claimed_at > 0 AND claimed_at <= ?)`
	}
	return w + `)`
}

// selectClaimableIDsSQL builds the claimable-ID SELECT. Bind order:
// partition_key, claim_version, [stale_cutoff_ms], limit.
func selectClaimableIDsSQL(staleEnabled bool) string {
	return `SELECT id FROM outbox
		 WHERE partition_key = ? AND ` + claimableWhere(staleEnabled) + `
		 ORDER BY created_at, seq
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
// claimableWhere fence (I2: the claim UPDATE is guarded, not a blind id-list
// write) so a row that stopped being claimable between select and update is
// never stolen.
//
// ponytail: an in-place upgrade leaves existing in-flight rows at
// first_attempted_at = 0, so their replay-budget clock starts at the FIRST
// post-upgrade claim rather than their true first attempt. This is fail-safe —
// it only ever grants MORE budget, never a premature poison — and legacy rows
// still fall back to the CreatedAt/poisonMinAge gate until re-claimed. Upgrade
// path if exactness is ever required: a one-time backfill of first_attempted_at
// from created_at for rows where first_attempted_at = 0.
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
