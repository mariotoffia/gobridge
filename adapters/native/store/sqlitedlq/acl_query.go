package sqlitedlq

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// SQL strings and dynamic-clause builders for the DLQ table.
//
// This file is the SQL-construction half of the SQLite ACL: it owns
// every query string the adapter sends to the driver. Row scanning
// lives in acl_row.go and lifecycle/exec wiring lives in
// acl_session.go.

// schemaSQL is the DDL run on every NewStore. Idempotent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS dlq (
    id              TEXT PRIMARY KEY,
    route_id        TEXT NOT NULL,
    binding_id      TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    source_id       TEXT NOT NULL DEFAULT '',
    correlation_id  TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    envelope_json   TEXT NOT NULL,
    failed_at       INTEGER NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    replayed        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dlq_route_id ON dlq(route_id);
CREATE INDEX IF NOT EXISTS idx_dlq_category ON dlq(category);
CREATE INDEX IF NOT EXISTS idx_dlq_failed_at ON dlq(failed_at);
`

// dlqColumns is the canonical column list used for SELECTs that
// hydrate full routing.DLQEntry values via scanEntries.
const dlqColumns = `id, route_id, binding_id, session_id, source_id, correlation_id,
		reason, category, error_code, last_error, envelope_json, failed_at, attempts`

const (
	insertDLQSQL = `INSERT INTO dlq (id, route_id, binding_id, session_id, source_id,
		 correlation_id, reason, category, error_code, last_error,
		 envelope_json, failed_at, attempts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	selectByIDSQL = `SELECT ` + dlqColumns + ` FROM dlq WHERE id = ?`

	purgeBeforeSQL = `DELETE FROM dlq WHERE failed_at < ?`
)

// filterClauses translates a routing.DLQFilter into SQL WHERE fragments
// + their bound arguments. Returns an empty slice if no clauses apply.
func filterClauses(filter routing.DLQFilter) (clauses []string, args []any) {
	if filter.RouteID != "" {
		clauses = append(clauses, "route_id = ?")
		args = append(args, filter.RouteID)
	}
	if filter.Category != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, filter.Category)
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "failed_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if !filter.Before.IsZero() {
		clauses = append(clauses, "failed_at < ?")
		args = append(args, filter.Before.UnixMilli())
	}
	return clauses, args
}

// listSQL builds the SELECT statement that drives Store.List from a
// filter, returning the SQL string and the appended argument list.
func listSQL(filter routing.DLQFilter) (string, []any) {
	clauses, args := filterClauses(filter)

	q := "SELECT " + dlqColumns + " FROM dlq"
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY failed_at DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	return q, args
}

// deleteByFilterSQL builds the DELETE statement that drives
// Store.DeleteByFilter. SQLite's DELETE does not always support LIMIT,
// so a bounded delete is expressed via a sub-SELECT.
func deleteByFilterSQL(filter routing.DLQFilter) (string, []any) {
	clauses, args := filterClauses(filter)

	if filter.Limit > 0 {
		sub := "SELECT id FROM dlq"
		if len(clauses) > 0 {
			sub += " WHERE " + strings.Join(clauses, " AND ")
		}
		sub += " ORDER BY failed_at DESC"
		sub += fmt.Sprintf(" LIMIT %d", filter.Limit)
		return "DELETE FROM dlq WHERE id IN (" + sub + ")", args
	}

	q := "DELETE FROM dlq"
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	return q, args
}

// deleteByIDsSQL builds the DELETE statement that removes n records
// by id.
func deleteByIDsSQL(n int) string {
	return fmt.Sprintf(`DELETE FROM dlq WHERE id IN (%s)`, placeholders(n))
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
