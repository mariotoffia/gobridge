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
    replay_count   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    completed_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(envelope_id, binding_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
CREATE TABLE IF NOT EXISTS outbox_partition_fence (
    partition_key TEXT PRIMARY KEY,
    max_version   INTEGER NOT NULL DEFAULT 0
);
`

// outboxColumns is the canonical column list used for SELECTs that
// hydrate full persistence.OutboxRecord values via scanRecords.
const outboxColumns = `id, partition_key, route_id, envelope_id, binding_id, session_id,
        address, envelope_json, headers_json, status, claimed_by, claim_version,
        replay_count, created_at, expires_at, completed_at`

const (
	insertOutboxSQL = `INSERT INTO outbox (id, partition_key, route_id, envelope_id, binding_id,
		 session_id, address, envelope_json, headers_json, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`

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
	// never lower the fence.
	upsertFenceVersionSQL = `INSERT INTO outbox_partition_fence (partition_key, max_version)
		 VALUES (?, ?)
		 ON CONFLICT(partition_key) DO UPDATE SET max_version = MAX(max_version, excluded.max_version)`

	selectPendingByPartitionSQL = `SELECT ` + outboxColumns + `
		 FROM outbox
		 WHERE partition_key = ? AND status = 'pending'
		 ORDER BY created_at, envelope_id
		 LIMIT ?`

	expireOutboxSQL = `UPDATE outbox SET status = 'expired'
		 WHERE expires_at > 0 AND expires_at < ? AND status = 'pending'`
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
		 ORDER BY created_at, envelope_id
		 LIMIT ?`
}

// updateClaimSQL builds the UPDATE statement that flips n records
// from pending/claimed to claimed under a new owner+version and stamps
// claimed_at so a later stale-claim reclaim can find a stranded row. The
// WHERE clause repeats the claimableWhere fence (I2: the claim UPDATE is
// guarded, not a blind id-list write) so a row that stopped being claimable
// between select and update is never stolen. Bind order:
// claimed_by, claim_version, claimed_at, ids..., claim_version, [stale_cutoff_ms].
func updateClaimSQL(n int, staleEnabled bool) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'claimed', claimed_by = ?, claim_version = ?,
			 claimed_at = ?, replay_count = replay_count + 1
			 WHERE id IN (%s) AND `+claimableWhere(staleEnabled),
		placeholders(n),
	)
}

// selectByIDsSQL builds the SELECT that hydrates n records by ID.
func selectByIDsSQL(n int) string {
	return fmt.Sprintf(
		`SELECT %s FROM outbox WHERE id IN (%s) ORDER BY created_at, envelope_id`,
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
// same fence as updateCompleteSQL). It clears claimed_by so the row is
// re-claimable on the next drain; claim_version is left as-is (it is
// irrelevant once pending and is overwritten by the next Claim). The
// outbox schema has no claimed_at column, so no claim timestamp is
// reset. replay_count is untouched — the next Claim increments it,
// preserving the poison-message cap.
func releaseOutboxSQL(n int) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'pending', claimed_by = ''
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
