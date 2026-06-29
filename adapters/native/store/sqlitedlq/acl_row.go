package sqlitedlq

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// Row-scanning ACL: translate *sql.Rows into routing.DLQEntry values.
// Lives here (not in acl_session.go) so the SDK→domain mapping is
// reviewable in isolation.

// scanDLQEntries drains rows into a slice of DLQEntry. The caller is
// responsible for closing rows.
func scanDLQEntries(rows *sql.Rows) ([]routing.DLQEntry, error) {
	result := make([]routing.DLQEntry, 0)
	for rows.Next() {
		var (
			spec       routing.DLQEntrySpec
			envJSON    string
			failedAtMs int64
		)
		err := rows.Scan(
			&spec.ID, &spec.RouteID, &spec.BindingID, &spec.SessionID, &spec.SourceID,
			&spec.CorrelationID, &spec.Reason, &spec.Category, &spec.ErrorCode, &spec.LastError,
			&envJSON, &failedAtMs, &spec.Attempts,
		)
		if err != nil {
			return nil, wrapErr(err, "sqlitedlq: scan entry")
		}

		spec.FailedAt = time.UnixMilli(failedAtMs)

		if err := json.Unmarshal([]byte(envJSON), &spec.Envelope); err != nil {
			return nil, fmt.Errorf("sqlitedlq: unmarshal envelope: %w", err)
		}

		// RehydrateDLQEntry (not NewDLQEntry): the envelope was freshly
		// decoded from the row and is already owned, so the entry takes it
		// without a redundant clone. Each scanned entry therefore has an
		// envelope independent of stored bytes (finding #9 read boundary).
		result = append(result, routing.RehydrateDLQEntry(spec))
	}

	if err := rows.Err(); err != nil {
		return nil, wrapErr(err, "sqlitedlq: rows err scan")
	}
	return result, nil
}
