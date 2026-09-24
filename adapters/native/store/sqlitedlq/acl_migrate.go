package sqlitedlq

import (
	"database/sql"
	"fmt"
)

// Columns added after the DLQ table's first release. A table created by an
// older release lacks them; migrate adds each column PRAGMA table_info does not
// report, so an existing DLQ file keeps its rows and gains the column with its
// default (a manual record with no redrive facts, ADR 0019).
func addedColumns() []struct{ name, ddl string } {
	return []struct{ name, ddl string }{
		{"redrive_mode", `ALTER TABLE dlq ADD COLUMN redrive_mode TEXT NOT NULL DEFAULT ''`},
		{"extra_info", `ALTER TABLE dlq ADD COLUMN extra_info TEXT NOT NULL DEFAULT '{}'`},
	}
}

// migrate wraps driver errors with %w so openSession's wrapErr still
// classifies them by their SQLite result code.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(dlq)`)
	if err != nil {
		return fmt.Errorf("read dlq columns: %w", err)
	}
	have := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read dlq columns: %w", err)
		}
		have[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read dlq columns: %w", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range addedColumns() {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}
