package sqlitedlq_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// preRedriveSchema is the DLQ table as a release before the redrive columns
// created it (ADR 0019). It must never change: it stands for files already on
// disk.
const preRedriveSchema = `
CREATE TABLE IF NOT EXISTS dlq (
    id              TEXT PRIMARY KEY,
    route_id        TEXT NOT NULL,
    binding_id      TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    source_id       TEXT NOT NULL DEFAULT '',
    correlation_id  TEXT NOT NULL DEFAULT '',
    address         TEXT NOT NULL DEFAULT '',
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

func openStoreForTest(t *testing.T, path string) *sqlitedlq.Store {
	t.Helper()
	s, err := sqlitedlq.NewStore(path)
	if err != nil {
		t.Fatalf("NewStore(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A DLQ file created by a release before the redrive columns existed opens,
// keeps its rows as manual records with no facts, and accepts new records with
// both fields. Opening it again is a no-op.
func TestOpenMigratesAPreRedriveTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dlq.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(preRedriveSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO dlq (id, route_id, envelope_json, failed_at) VALUES ('old-1', 'r1', '', 1700000000000)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStoreForTest(t, path)
	old, err := s.Get(t.Context(), "old-1")
	if err != nil {
		t.Fatalf("get pre-migration row: %v", err)
	}
	if old.RedriveMode() != routing.RedriveManual || old.ExtraInfo() != nil {
		t.Fatalf("old row: mode %q info %v, want manual and none", old.RedriveMode(), old.ExtraInfo())
	}
	fresh := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "new-1", RouteID: "r1", FailedAt: time.UnixMilli(1700000001000),
		RedriveMode: routing.RedriveAuto, ExtraInfo: map[string]string{routing.ExtraInfoSessionID: "s1"},
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-new-1", Subject: "a"}),
	})
	if err := s.Write(t.Context(), fresh); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
	got, err := s.Get(t.Context(), "new-1")
	if err != nil || got.RedriveMode() != routing.RedriveAuto || got.ExtraInfo()[routing.ExtraInfoSessionID] != "s1" {
		t.Fatalf("new row after migration: %v mode %q info %v", err, got.RedriveMode(), got.ExtraInfo())
	}
	// A second open finds both columns and adds nothing.
	_ = openStoreForTest(t, path)
}
