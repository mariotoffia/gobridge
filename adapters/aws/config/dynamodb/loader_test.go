package dynamodb_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

func newLoader(t *testing.T, prefix string) *ddbconfig.Loader {
	t.Helper()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable(prefix)
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(tableName),
		ddbconfig.WithBridgeID("test-bridge"),
	)
	if err := loader.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)
	return loader
}

func sampleConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "test-bridge",
			ShutdownTimeout: "10s",
			LogLevel:        "info",
		},
		Sessions: []ports.SessionDef{
			{
				ID:        "mqtt-1",
				Transport: "mqtt",
			},
		},
		Receivers: []ports.ReceiverDef{
			{
				ID:        "rx-1",
				Transport: "mqtt",
				SessionID: "mqtt-1",
				Topics: []ports.SubscriptionDef{
					{Topic: "sensors/#", QoS: 1},
				},
			},
		},
		Senders: []ports.SenderDef{
			{
				ID:        "tx-1",
				Transport: "mqtt",
				SessionID: "mqtt-1",
			},
		},
		Routes: []ports.RouteDef{
			{
				ID:         "route-1",
				ReceiverID: "rx-1",
				Bindings:   []string{"bind-1"},
			},
		},
	}
}

// Verifies Save followed by Load returns an equivalent bridge configuration.
func TestLoadSuccess(t *testing.T) {
	loader := newLoader(t, "cfg-load")
	ctx := context.Background()

	cfg := sampleConfig()
	if err := loader.Save(ctx, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got.Bridge.ID != cfg.Bridge.ID {
		t.Errorf("Bridge.ID: got %q, want %q", got.Bridge.ID, cfg.Bridge.ID)
	}
	if got.Bridge.LogLevel != cfg.Bridge.LogLevel {
		t.Errorf("Bridge.LogLevel: got %q, want %q", got.Bridge.LogLevel, cfg.Bridge.LogLevel)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("Sessions length: got %d, want 1", len(got.Sessions))
	}
	if got.Sessions[0].ID != "mqtt-1" {
		t.Errorf("Sessions[0].ID: got %q, want %q", got.Sessions[0].ID, "mqtt-1")
	}
	if len(got.Receivers) != 1 {
		t.Fatalf("Receivers length: got %d, want 1", len(got.Receivers))
	}
	if got.Receivers[0].Topics[0].Topic != "sensors/#" {
		t.Errorf("Receivers[0].Topics[0].Topic: got %q, want %q", got.Receivers[0].Topics[0].Topic, "sensors/#")
	}
	if len(got.Routes) != 1 {
		t.Fatalf("Routes length: got %d, want 1", len(got.Routes))
	}
	if got.Routes[0].ID != "route-1" {
		t.Errorf("Routes[0].ID: got %q, want %q", got.Routes[0].ID, "route-1")
	}
}

// Verifies Load returns ErrNotFound when no configuration row exists.
func TestLoadNotFound(t *testing.T) {
	loader := newLoader(t, "cfg-nf")
	ctx := context.Background()

	_, err := loader.Load(ctx)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Verifies Watch emits updated configuration after a subsequent Save changes the stored version.
func TestWatchDetectsChanges(t *testing.T) {
	loader := newLoader(t, "cfg-watch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg1 := sampleConfig()
	if err := loader.Save(ctx, cfg1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if _, err := loader.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("cfg-watchpoll")
	pollLoader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(tableName),
		ddbconfig.WithBridgeID("test-bridge"),
		ddbconfig.WithPollInterval(100*time.Millisecond),
	)
	if err := pollLoader.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	cfg1 = sampleConfig()
	if err := pollLoader.Save(ctx, cfg1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if _, err := pollLoader.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	ch, err := pollLoader.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	cfg2 := sampleConfig()
	cfg2.Bridge.LogLevel = "debug"
	if err := pollLoader.Save(ctx, cfg2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case got := <-ch:
		if got == nil {
			t.Fatal("received nil config from watch")
		}
		if got.Bridge.LogLevel != "debug" {
			t.Errorf("Bridge.LogLevel: got %q, want %q", got.Bridge.LogLevel, "debug")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for config change")
	}
}

// Verifies Watch does not emit when the stored configuration version is unchanged across polls.
func TestWatchNoDuplicates(t *testing.T) {
	fc := clocktest.New()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("cfg-nodup")
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(tableName),
		ddbconfig.WithBridgeID("test-bridge"),
		ddbconfig.WithPollInterval(100*time.Millisecond),
		ddbconfig.WithClock(fc),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := loader.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	cfg := sampleConfig()
	if err := loader.Save(ctx, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := loader.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	ch, err := loader.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	if got := fc.TickerCount(); got != 1 {
		t.Fatalf("expected poll ticker to be registered synchronously, got %d", got)
	}

	// Fire 5 poll cycles; no new version means no emission.
	fc.Advance(500 * time.Millisecond)
	// Yield so the goroutine can drain pending ticks.
	runtime.Gosched()

	select {
	case got := <-ch:
		if got != nil {
			t.Fatal("expected no config emission when version unchanged")
		}
	default:
		// expected: nothing emitted
	}
}

// Verifies repeated Save calls advance the stored version so the latest fields are visible on Load.
func TestSaveIncrementsVersion(t *testing.T) {
	loader := newLoader(t, "cfg-ver")
	ctx := context.Background()

	cfg := sampleConfig()
	if err := loader.Save(ctx, cfg); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	cfg.Bridge.LogLevel = "debug"
	if err := loader.Save(ctx, cfg); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	cfg.Bridge.LogLevel = "warn"
	if err := loader.Save(ctx, cfg); err != nil {
		t.Fatalf("save v3: %v", err)
	}

	got, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Bridge.LogLevel != "warn" {
		t.Errorf("Bridge.LogLevel: got %q, want %q", got.Bridge.LogLevel, "warn")
	}
}

// Verifies EnsureTable succeeds when invoked repeatedly for the same loader.
func TestEnsureTableIdempotent(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("cfg-etable")
	loader := ddbconfig.NewLoader(client, ddbconfig.WithTableName(tableName))
	ctx := context.Background()

	if err := loader.EnsureTable(ctx); err != nil {
		t.Fatalf("first EnsureTable: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	if err := loader.EnsureTable(ctx); err != nil {
		t.Fatalf("second EnsureTable should be idempotent: %v", err)
	}
}
