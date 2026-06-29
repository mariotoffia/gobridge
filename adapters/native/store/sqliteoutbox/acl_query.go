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
    replay_count   INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL DEFAULT 0,
    completed_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(envelope_id, binding_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_partition_status ON outbox(partition_key, status);
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

	// selectMaxClaimVersionSQL reports the highest claim_version ever
	// stamped on the partition, so Claim can reject a stale (older) token
	// version monotonically, matching the version-monotonic fence the
	// memory backend enforces and ports.OutboxStore documents.
	selectMaxClaimVersionSQL = `SELECT COALESCE(MAX(claim_version), 0)
		 FROM outbox WHERE partition_key = ?`

	selectClaimableIDsSQL = `SELECT id FROM outbox
		 WHERE partition_key = ? AND (status = 'pending' OR (status = 'claimed' AND claim_version < ?))
		 ORDER BY created_at, envelope_id
		 LIMIT ?`

	selectPendingByPartitionSQL = `SELECT ` + outboxColumns + `
		 FROM outbox
		 WHERE partition_key = ? AND status = 'pending'
		 ORDER BY created_at, envelope_id
		 LIMIT ?`

	expireOutboxSQL = `UPDATE outbox SET status = 'expired'
		 WHERE expires_at > 0 AND expires_at < ? AND status = 'pending'`
)

// updateClaimSQL builds the UPDATE statement that flips n records
// from pending/claimed to claimed under a new owner+version.
func updateClaimSQL(n int) string {
	return fmt.Sprintf(
		`UPDATE outbox SET status = 'claimed', claimed_by = ?, claim_version = ?,
			 replay_count = replay_count + 1
			 WHERE id IN (%s)`,
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
