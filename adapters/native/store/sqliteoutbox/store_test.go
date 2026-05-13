package sqliteoutbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

func newTempStore(t *testing.T) *sqliteoutbox.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "outbox.db")
	s, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Validates the SQLite outbox store against the shared conformance suite.
func TestOutboxStoreConformance(t *testing.T) {
	store := newTempStore(t)
	storetest.RunOutboxStoreTests(t, store)
}

// Validates the in-memory SQLite outbox store against the shared conformance suite.
func TestInMemoryMode(t *testing.T) {
	s, err := sqliteoutbox.NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()
	storetest.RunOutboxStoreTests(t, s)
}

// Verifies persisted outbox records survive closing and reopening the database file.
func TestDurability_CloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "durable.db")

	s1, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "dur-1",
		RouteID:    "route-1",
		EnvelopeID: "env-dur-1",
		BindingID:  "bind-dur-1",
		SessionID:  "sess-dur",
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-dur-1",
			Subject: "test",
			Payload: []byte("durable payload"),
		}),
	})
	if err := s1.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	pending, err := s2.QueryPending(ctx, "SESSION#sess-dur", 10)
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 record after reopen, got %d", len(pending))
	}
	if pending[0].ID != "dur-1" {
		t.Fatalf("id: got %q, want %q", pending[0].ID, "dur-1")
	}
	if string(pending[0].Envelope.Payload) != "durable payload" {
		t.Fatalf("payload mismatch: %q", pending[0].Envelope.Payload)
	}
}

// Verifies the database file remains after Close following a persist.
func TestTempFileCleanup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cleanup.db")

	s, err := sqliteoutbox.NewStore(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "tmp-1",
		RouteID:    "route-1",
		EnvelopeID: "env-tmp-1",
		BindingID:  "bind-tmp-1",
		SessionID:  "sess-tmp",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-tmp-1", Subject: "test"}),
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("db file should exist after close")
	}
}

// Verifies dispatch headers round-trip through persist and query.
func TestDispatchHeadersRoundTrip(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "hdr-1",
		RouteID:    "route-1",
		EnvelopeID: "env-hdr-1",
		BindingID:  "bind-hdr-1",
		SessionID:  "sess-hdr",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-hdr-1", Subject: "test"}),
		DispatchHeaders: map[string]any{
			"x-custom":  "value",
			"x-numeric": float64(42),
		},
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-hdr", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if v, ok := pending[0].DispatchHeaders["x-custom"]; !ok || v != "value" {
		t.Fatalf("dispatch header x-custom: %v", pending[0].DispatchHeaders)
	}
}

// Verifies WithClock controls default CreatedAt timestamps for persisted records.
func TestWithClockControlsCreatedAt(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 5, 4, 12, 30, 45, 123000000, time.UTC))
	s, err := sqliteoutbox.NewStore(":memory:", sqliteoutbox.WithClock(clk))
	if err != nil {
		t.Fatalf("NewStore(:memory:): %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "clk-1",
		RouteID:    "route-1",
		EnvelopeID: "env-clk-1",
		BindingID:  "bind-clk-1",
		SessionID:  "sess-clk",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-clk-1", Subject: "test"}),
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-clk", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if !pending[0].CreatedAt.Equal(clk.Now()) {
		t.Fatalf("createdAt: got %v, want %v", pending[0].CreatedAt, clk.Now())
	}
}

// Verifies ExpiresAt round-trips through persist and query.
func TestExpiresAtRoundTrip(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "exprt-1",
		RouteID:    "route-1",
		EnvelopeID: "env-exprt-1",
		BindingID:  "bind-exprt-1",
		SessionID:  "sess-exprt",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-exprt-1", Subject: "test"}),
		ExpiresAt:  expiry,
	})
	if err := s.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := s.QueryPending(ctx, "SESSION#sess-exprt", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if !pending[0].ExpiresAt.Equal(expiry) {
		t.Fatalf("expiresAt: got %v, want %v", pending[0].ExpiresAt, expiry)
	}
}
