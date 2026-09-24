package sqlitedlq

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Two processes may open the same pre-redrive file at once: both read the
// columns as missing, one ALTER wins, and the other must still open. Driving
// the add step with an empty "have" set against a table that already has the
// columns replays the losing side of that race deterministically.
func TestMigrateTreatsAColumnAddedByAnotherOpenAsDone(t *testing.T) {
	s := mustStore(t, filepath.Join(t.TempDir(), "dlq.db"))

	if err := addMissingColumns(s.sess.db, map[string]bool{}); err != nil {
		t.Fatalf("add step after another open added the columns: %v", err)
	}
	for _, c := range addedColumns() {
		var n int
		if err := s.sess.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('dlq') WHERE name = ?`, c.name).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.name, err)
		}
		if n != 1 {
			t.Fatalf("column %s appears %d times, want 1", c.name, n)
		}
	}
}

// An ALTER that fails while the column is still absent is a real fault, and
// the open must report it rather than start without the column.
func TestMigrateReportsAnAlterThatDidNotAddTheColumn(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "no-table.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := addMissingColumns(db, map[string]bool{}); err == nil {
		t.Fatal("add step against a missing dlq table returned nil, want the ALTER error")
	}
}
