package sqliteoutbox

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Row-scanning ACL: translate *sql.Rows into persistence.OutboxRecord
// aggregates via OutboxSnapshot. Lives here (not in acl_session.go) so
// the SDK→domain mapping is reviewable in isolation.

// scanOutboxRecords drains rows into a slice of OutboxRecord aggregates
// rehydrated from snapshots. The caller is responsible for closing rows.
func scanOutboxRecords(rows *sql.Rows) ([]*persistence.OutboxRecord, error) {
	var result []*persistence.OutboxRecord
	for rows.Next() {
		var (
			snap        persistence.OutboxSnapshot
			pk          string
			envJSON     string
			headersJSON sql.NullString
			status      string
			claimedAtMs int64
			createdAtMs int64
			expiresAtMs int64
			completedMs int64
		)
		err := rows.Scan(
			&snap.ID, &pk, &snap.RouteID, &snap.EnvelopeID, &snap.BindingID, &snap.SessionID,
			&snap.Address, &envJSON, &headersJSON, &status, &snap.ClaimedBy, &snap.ClaimVersion,
			&claimedAtMs, &snap.ReplayCount, &createdAtMs, &expiresAtMs, &completedMs, &snap.Seq,
		)
		if err != nil {
			return nil, wrapErr(err, "sqliteoutbox: scan record")
		}

		snap.Status = persistence.OutboxStatus(status)
		snap.CreatedAt = time.UnixMilli(createdAtMs)
		if claimedAtMs > 0 {
			snap.ClaimedAt = time.UnixMilli(claimedAtMs)
		}
		if expiresAtMs > 0 {
			snap.ExpiresAt = time.UnixMilli(expiresAtMs)
		}
		if completedMs > 0 {
			snap.CompletedAt = time.UnixMilli(completedMs)
		}

		if err := json.Unmarshal([]byte(envJSON), &snap.Envelope); err != nil {
			return nil, fmt.Errorf("sqliteoutbox: unmarshal envelope: %w", err)
		}
		if headersJSON.Valid && headersJSON.String != "" {
			if err := json.Unmarshal([]byte(headersJSON.String), &snap.DispatchHeaders); err != nil {
				return nil, fmt.Errorf("sqliteoutbox: unmarshal headers: %w", err)
			}
		}

		result = append(result, persistence.RehydrateFromSnapshot(snap))
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(err, "sqliteoutbox: rows err scan")
	}
	return result, nil
}

// nullableString returns a NullString that is valid iff b is non-nil.
func nullableString(b []byte) sql.NullString {
	if b == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
