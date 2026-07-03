package sqlitedlq

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// legacySchemaSQL is the pre-address DDL as shipped before the Address
// column was persisted. Used to simulate a database created by an older
// binary.
const legacySchemaSQL = `
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
`

// Verifies NewStore migrates a legacy database (no address column) in
// place: pre-existing rows hydrate with an empty Address and new writes
// persist Address durably.
func TestLegacySchemaGainsAddressColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(legacySchemaSQL); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO dlq (id, route_id, envelope_json, failed_at, attempts)
		 VALUES (?, ?, ?, ?, ?)`,
		"legacy-1", "route-L",
		`{"id":"env-legacy-1","subject":"t/s","payload":"e30=","headers":{}}`,
		time.Now().Add(-time.Hour).UnixMilli(), 1,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore on legacy db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	got, err := s.Get(ctx, "legacy-1")
	if err != nil {
		t.Fatalf("Get legacy row: %v", err)
	}
	if got.Address() != "" {
		t.Fatalf("legacy row Address: got %q, want empty", got.Address())
	}

	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:      "new-1",
		RouteID: "route-L",
		Address: "amqp://queue/orders",
		Reason:  "test",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-new-1",
			Subject: "t/s",
			Payload: []byte(`{}`),
		}),
		FailedAt: time.Now(),
		Attempts: 1,
	})
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("Write after migration: %v", err)
	}

	round, err := s.Get(ctx, "new-1")
	if err != nil {
		t.Fatalf("Get new row: %v", err)
	}
	if round.Address() != "amqp://queue/orders" {
		t.Fatalf("Address not persisted: got %q", round.Address())
	}
}
