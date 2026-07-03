package nativestore_test

import (
	"context"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies the memory store factory returns a non-nil lease store.
func TestMemoryStoreFactory_NewLeaseStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

// Verifies the memory store factory returns a non-nil outbox store.
func TestMemoryStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the memory store factory returns a non-nil DLQ store.
func TestMemoryStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()
	s, err := f.NewDLQStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies the SQLite store factory returns an error for the unsupported lease role.
func TestSQLiteStoreFactory_NewLeaseStore_ReturnsError(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for unimplemented SQLite lease store")
	}
	if s != nil {
		t.Fatal("expected nil LeaseStore when error is returned")
	}
}

// Verifies the SQLite factory builds an outbox store from a typed config.
func TestSQLiteStoreFactory_NewOutboxStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	s, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the SQLite factory accepts the runtime-derived StaleClaimDuration
// (I1 wiring). Deterministic stale-reclaim behaviour is covered with a fake
// clock in the sqliteoutbox package; here we only pin that the factory wires
// the option through and still constructs a usable store.
func TestSQLiteStoreFactory_NewOutboxStore_WithStaleClaimDuration(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	s, err := f.NewOutboxStore(context.Background(), cfg,
		ports.OutboxRuntimeOptions{StaleClaimDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies the SQLite factory builds a DLQ store from a typed config.
func TestSQLiteStoreFactory_NewDLQStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	s, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies SQLite outbox and DLQ construction fail when the typed config is missing.
func TestSQLiteStoreFactory_MissingPath(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()

	_, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if err == nil {
		t.Fatal("expected error for missing typed config")
	}

	_, err = f.NewDLQStore(context.Background(), &nativestore.SQLiteConfig{})
	if err == nil {
		t.Fatal("expected error for empty Path in typed config")
	}
}

// Verifies the typed config's stale_claim_duration and retention knobs
// are accepted by the factory (typed stale-claim overrides the
// runtime-derived value; deterministic behaviour of both knobs is pinned
// with a fake clock inside the sqliteoutbox package).
func TestSQLiteStoreFactory_TypedTuningKnobs(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	cfg := &nativestore.SQLiteConfig{
		Path:               ":memory:",
		StaleClaimDuration: time.Minute,
		Retention:          2 * time.Hour,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	s, err := f.NewOutboxStore(context.Background(), cfg,
		ports.OutboxRuntimeOptions{StaleClaimDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies a negative retention (compaction disabled) constructs, while a
// negative stale_claim_duration is rejected by Validate.
func TestSQLiteConfig_ValidateDurations(t *testing.T) {
	ok := nativestore.SQLiteConfig{Path: ":memory:", Retention: -1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("negative retention should validate (disables compaction): %v", err)
	}

	bad := nativestore.SQLiteConfig{Path: ":memory:", StaleClaimDuration: -time.Second}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected validation error for negative stale_claim_duration")
	}
}
