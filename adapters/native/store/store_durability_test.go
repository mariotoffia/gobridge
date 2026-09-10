package nativestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ackVolatile is the memory config every volatile-store construction in this
// file needs: the in-memory outbox and DLQ hold accepted work and terminal
// evidence in process memory only, so the factory refuses to build them until
// the operator has acknowledged that restart loses both.
func ackVolatile() *nativestore.MemoryConfig {
	return &nativestore.MemoryConfig{AcknowledgeVolatile: true}
}

// Verifies the memory outbox refuses to build until the operator acknowledges
// that its contents do not survive the process. A persist-before-ack route on
// an unacknowledged volatile outbox looks crash-durable to the source and is
// not, so construction must fail closed rather than warn.
func TestMemoryStoreFactory_NewOutboxStoreRequiresVolatileAcknowledgement(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()

	for _, cfg := range []ports.PluginConfig{nil, &nativestore.MemoryConfig{}, nativestore.MemoryConfig{}} {
		s, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{})
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig for unacknowledged volatile outbox (cfg %T), got %v", cfg, err)
		}
		if !strings.Contains(err.Error(), "acknowledge_volatile") {
			t.Fatalf("the error must name the config key that unblocks it, got %v", err)
		}
		if s != nil {
			t.Fatalf("expected nil OutboxStore when construction fails (cfg %T)", cfg)
		}
	}

	s, err := f.NewOutboxStore(context.Background(), ackVolatile(), ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error with acknowledgement: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore once volatility is acknowledged")
	}
}

// Verifies the memory DLQ refuses to build until volatility is acknowledged:
// the DLQ is the terminal evidence of dropped work, and losing it on restart
// erases the only record that the work existed.
func TestMemoryStoreFactory_NewDLQStoreRequiresVolatileAcknowledgement(t *testing.T) {
	f := nativestore.NewMemoryStoreFactory()

	for _, cfg := range []ports.PluginConfig{nil, &nativestore.MemoryConfig{}, nativestore.MemoryConfig{}} {
		s, err := f.NewDLQStore(context.Background(), cfg)
		if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig for unacknowledged volatile DLQ (cfg %T), got %v", cfg, err)
		}
		if !strings.Contains(err.Error(), "acknowledge_volatile") {
			t.Fatalf("the error must name the config key that unblocks it, got %v", err)
		}
		if s != nil {
			t.Fatalf("expected nil DLQStore when construction fails (cfg %T)", cfg)
		}
	}

	s, err := f.NewDLQStore(context.Background(), ackVolatile())
	if err != nil {
		t.Fatalf("unexpected error with acknowledgement: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DLQStore once volatility is acknowledged")
	}
}

// Verifies both native factories declare their crash-durability truthfully.
// The composition guard reads this capability to reject a volatile lease under
// a durable fenced outbox, so a factory that lies here defeats the guard.
func TestNativeStoreFactory_DeclaresCrashDurability(t *testing.T) {
	var mem ports.CrashDurableStoreFactory = nativestore.NewMemoryStoreFactory()
	if mem.IsCrashDurable() {
		t.Fatal("the in-memory store factory must report NOT crash-durable")
	}

	var sqlite ports.CrashDurableStoreFactory = nativestore.NewSQLiteStoreFactory()
	if !sqlite.IsCrashDurable() {
		t.Fatal("the SQLite store factory must report crash-durable")
	}
}

// Proves WHY a volatile lease may not back a durable fenced outbox: the
// in-memory lease numbers fencing versions from a per-process counter, so a
// restart re-issues version 1 while the SQLite outbox still holds the
// partition fence its predecessor raised. Every claim by the new owner is then
// rejected as stale and the partition never drains again — ingress keeps
// acknowledging into a backlog nobody can take.
func TestStoreFactory_MemoryLeaseVersionResetWedgesDurableOutboxFence(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "outbox.db")

	sqliteCfg := &nativestore.SQLiteConfig{Path: dbPath}
	outbox, err := nativestore.NewSQLiteStoreFactory().
		NewOutboxStore(ctx, sqliteCfg, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("build sqlite outbox: %v", err)
	}
	t.Cleanup(func() { closeStore(t, outbox) })

	memCfg := &nativestore.MemoryConfig{AcknowledgeSingleReplica: true}
	lease, err := nativestore.NewMemoryStoreFactory().NewLeaseStore(ctx, memCfg)
	if err != nil {
		t.Fatalf("build memory lease: %v", err)
	}

	// The first process acquires twice (a re-acquire after any lease loss),
	// raising the durable partition fence to version 2.
	first, err := lease.Acquire(ctx, "route-1", "owner-a", time.Minute, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := lease.Release(ctx, "route-1", first); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := lease.Acquire(ctx, "route-1", "owner-a", time.Minute, nil)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Version <= first.Version {
		t.Fatalf("expected a re-acquire to advance the fencing version, got %d then %d", first.Version, second.Version)
	}
	if _, err := outbox.Claim(ctx, "route-1", second, 0); err != nil {
		t.Fatalf("fencing no-op claim at version %d: %v", second.Version, err)
	}

	// The process restarts: the durable outbox keeps its fence, the in-memory
	// lease starts counting from zero again and hands out version 1.
	restarted, err := nativestore.NewMemoryStoreFactory().NewLeaseStore(ctx, memCfg)
	if err != nil {
		t.Fatalf("rebuild memory lease: %v", err)
	}
	afterRestart, err := restarted.Acquire(ctx, "route-1", "owner-b", time.Minute, nil)
	if err != nil {
		t.Fatalf("acquire after restart: %v", err)
	}
	if afterRestart.Version >= second.Version {
		t.Fatalf("expected the in-memory lease to regress below the durable fence, got %d (durable high-water %d)",
			afterRestart.Version, second.Version)
	}

	_, err = outbox.Claim(ctx, "route-1", afterRestart, 10)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected the durable fence to reject the regressed lease version, got %v", err)
	}
}

// closeStore releases a store handle that implements io.Closer.
func closeStore(t *testing.T, s any) {
	t.Helper()
	if c, ok := s.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
}
