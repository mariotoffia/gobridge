package sqliteoutbox

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// Row-scanning ACL: translate *sql.Rows into domain.OutboxRecord
// values. Lives here (not in acl_session.go) so the SDK→domain
// mapping is reviewable in isolation.

// scanOutboxRecords drains rows into a slice of OutboxRecord. The
// caller is responsible for closing rows.
func scanOutboxRecords(rows *sql.Rows) ([]domain.OutboxRecord, error) {
	var result []domain.OutboxRecord
	for rows.Next() {
		var (
			r           domain.OutboxRecord
			pk          string
			envJSON     string
			headersJSON sql.NullString
			status      string
			createdAtMs int64
			expiresAtMs int64
			completedMs int64
		)
		err := rows.Scan(
			&r.ID, &pk, &r.RouteID, &r.EnvelopeID, &r.BindingID, &r.SessionID,
			&r.Address, &envJSON, &headersJSON, &status, &r.ClaimedBy, &r.ClaimVersion,
			&r.ReplayCount, &createdAtMs, &expiresAtMs, &completedMs,
		)
		if err != nil {
			return nil, wrapErr(err, "sqliteoutbox: scan record")
		}

		r.Status = domain.OutboxStatus(status)
		r.CreatedAt = time.UnixMilli(createdAtMs)
		if expiresAtMs > 0 {
			r.ExpiresAt = time.UnixMilli(expiresAtMs)
		}
		if completedMs > 0 {
			r.CompletedAt = time.UnixMilli(completedMs)
		}

		if err := json.Unmarshal([]byte(envJSON), &r.Envelope); err != nil {
			return nil, fmt.Errorf("sqliteoutbox: unmarshal envelope: %w", err)
		}
		if headersJSON.Valid && headersJSON.String != "" {
			if err := json.Unmarshal([]byte(headersJSON.String), &r.DispatchHeaders); err != nil {
				return nil, fmt.Errorf("sqliteoutbox: unmarshal headers: %w", err)
			}
		}

		result = append(result, r)
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
