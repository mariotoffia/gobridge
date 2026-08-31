package nativestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies the memory lease store fails fast when single-replica operation is
// not acknowledged (nil config or the flag left false), and builds once it is.
// An unacknowledged in-memory lease behind more than one replica would silently
// split the brain — every replica believing it owns the same exclusive session
// — so construction must refuse it rather than warn.
func TestMemoryStoreFactory_NewLeaseStore(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()

	// Not acknowledged -> fail closed. Mutation guard: dropping the gate in
	// NewLeaseStore makes these return a non-nil store with no error.
	for _, cfg := range []ports.PluginConfig{nil, &nativestore.MemoryConfig{}, nativestore.MemoryConfig{}} {
		s, err := f.NewLeaseStore(context.Background(), cfg)
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig for unacknowledged single-replica lease (cfg %T), got %v", cfg, err)
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

// Verifies the SQLite store factory reports the unsupported lease role as
// ErrNotSupported — a missing capability, not a malformed config, so a caller
// can tell "this backend has no lease store" from "your lease config is wrong".
func TestSQLiteStoreFactory_NewLeaseStore_ReturnsError(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	s, err := f.NewLeaseStore(context.Background(), nil)
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported for unimplemented SQLite lease store, got %v", err)
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
// Deterministic stale-reclaim behaviour is covered with a fake
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
	store, err := f.NewManagedSubscriptionStore(t.Context(), &nativestore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "adapter-owned", "managed.db")})
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

// Verifies SQLite outbox and DLQ construction reject a missing typed config and
// an empty Path as ErrInvalidConfig, so the composition root can tell an
// operator config error from a store I/O failure.
func TestSQLiteStoreFactory_MissingPath(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()

	_, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for missing typed config, got %v", err)
	}

	_, err = f.NewDLQStore(context.Background(), &nativestore.SQLiteConfig{})
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty Path in typed config, got %v", err)
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

func TestSQLiteStoreFactory_NewManagedSubscriptionStoreRejectsSQLiteURI(t *testing.T) {
	target := filepath.Join(t.TempDir(), "escaped.db")
	store, err := nativestore.NewSQLiteStoreFactory().NewManagedSubscriptionStore(
		t.Context(), &nativestore.SQLiteConfig{Path: "file:" + target + "?mode=rwc"},
	)
	if store != nil {
		t.Fatal("SQLite URI returned a managed subscription store")
	}
	if err == nil {
		t.Fatal("SQLite URI must fail managed subscription store validation")
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("URI bypass created target database: %v", statErr)
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
