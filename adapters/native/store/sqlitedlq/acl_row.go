package sqlitedlq

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// Row-scanning ACL: translate *sql.Rows into domain.DLQEntry values.
// Lives here (not in acl_session.go) so the SDK→domain mapping is
// reviewable in isolation.

// scanDLQEntries drains rows into a slice of DLQEntry. The caller is
// responsible for closing rows.
func scanDLQEntries(rows *sql.Rows) ([]domain.DLQEntry, error) {
	result := make([]domain.DLQEntry, 0)
	for rows.Next() {
		var (
			e          domain.DLQEntry
			envJSON    string
			failedAtMs int64
		)
		err := rows.Scan(
			&e.ID, &e.RouteID, &e.BindingID, &e.SessionID, &e.SourceID,
			&e.CorrelationID, &e.Reason, &e.Category, &e.ErrorCode, &e.LastError,
			&envJSON, &failedAtMs, &e.Attempts,
		)
		if err != nil {
			return nil, wrapErr(err, "sqlitedlq: scan entry")
		}

		e.FailedAt = time.UnixMilli(failedAtMs)

		if err := json.Unmarshal([]byte(envJSON), &e.Envelope); err != nil {
			return nil, fmt.Errorf("sqlitedlq: unmarshal envelope: %w", err)
		}

		result = append(result, e)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapErr(err, "sqlitedlq: rows err scan")
	}
	return result, nil
}
