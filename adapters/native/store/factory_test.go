package nativestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies the memory lease store fails fast when single-replica operation is
// not acknowledged (nil config or the flag left false), and builds once it is.
// This is the c10-memlease-split gate: an unacknowledged in-memory lease behind
// >1 replica would silently split-brain, so construction must refuse it.
func TestMemoryStoreFactory_NewLeaseStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()

	// Not acknowledged -> fail closed. Mutation guard: dropping the gate in
	// NewLeaseStore makes these return a non-nil store with no error.
	for _, cfg := range []ports.PluginConfig{nil, &nativestore.MemoryConfig{}, nativestore.MemoryConfig{}} {
		s, err := f.NewLeaseStore(context.Background(), cfg)
		if err == nil {
			t.Fatalf("expected fail-closed error for unacknowledged single-replica lease (cfg %T)", cfg)
		}
		if s != nil {
			t.Fatalf("expected nil LeaseStore when construction fails (cfg %T)", cfg)
		}
	}

	// Acknowledged -> constructs.
	s, err := f.NewLeaseStore(context.Background(),
		&nativestore.MemoryConfig{AcknowledgeSingleReplica: true})
	if err != nil {
		t.Fatalf("unexpected error with acknowledgement: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil LeaseStore once single-replica is acknowledged")
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

// Verifies the SQLite factory exposes the optional managed-subscription role.
func TestSQLiteStoreFactory_NewManagedSubscriptionStore(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	store, err := f.NewManagedSubscriptionStore(t.Context(), &nativestore.SQLiteConfig{Path: t.TempDir() + "/managed.db"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil ManagedSubscriptionStore")
	}
	if err := store.Remember(t.Context(), "identity", []string{"sensors/#"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	filters, err := store.List(t.Context(), "identity")
	if err != nil || len(filters) != 1 || filters[0] != "sensors/#" {
		t.Fatalf("List = %v, %v", filters, err)
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

// Verifies SQLite outbox, DLQ, and managed-subscription construction fail when the typed config is missing.
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

func TestSQLiteStoreFactory_NewManagedSubscriptionStoreHonorsCanceledBuildContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", "managed.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(
		ctx, &nativestore.SQLiteConfig{Path: path},
	)
	if store != nil {
		t.Fatal("canceled build returned a store")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, statErr := os.Lstat(candidate); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled build created %q: %v", candidate, statErr)
		}
	}
	if _, statErr := os.Lstat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled build created parent directory: %v", statErr)
	}
}
