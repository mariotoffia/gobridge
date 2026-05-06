package integration_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// fakeOverlayConfig is defined in zz_register_fakes_test.go and
// registered as the kind "fake" decoder so options round-trip through
// DDB save/load and ParseFile.

// ===============================================================
// Group 1: DynamoDB Overlay on File Config
//
// Validates that config.Manager.Load() correctly merges a file
// base layer with a DynamoDB overlay layer using DefaultMerge.
// ===============================================================

// TestDDBOverlay_MergesSessionsFromDDB validates that a session
// defined only in the DynamoDB overlay appears in the merged config.
func TestDDBOverlay_MergesSessionsFromDDB(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-sess")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	overlay.Sessions = []ports.SessionDef{
		{ID: "mqtt-1", Transport: "fake", Config: fakeOverlayConfig{Broker: "tcp://localhost:1883"}},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Sessions) != 1 {
		t.Fatalf("Sessions: got %d, want 1", len(cfg.Sessions))
	}
	if cfg.Sessions[0].ID != "mqtt-1" {
		t.Errorf("Sessions[0].ID: got %q, want %q", cfg.Sessions[0].ID, "mqtt-1")
	}
}

// TestDDBOverlay_ReplacesSessionByID validates that an overlay
// session with the same ID as a base session replaces it.
func TestDDBOverlay_ReplacesSessionByID(t *testing.T) {
	basePath := writeBaseYAMLWithSession(t, "test-bridge", "s1", "fake")
	loader := ddbConfigLoader(t, "overlay-srepl")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	overlay.Sessions = []ports.SessionDef{
		{ID: "s1", Transport: "fake", Config: fakeOverlayConfig{Broker: "tcp://new-host:1883"}},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Sessions) != 1 {
		t.Fatalf("Sessions: got %d, want 1", len(cfg.Sessions))
	}
	merged, ok := cfg.Sessions[0].Config.(fakeOverlayConfig)
	if !ok {
		t.Fatalf("expected fakeOverlayConfig, got %T", cfg.Sessions[0].Config)
	}
	if merged.Broker != "tcp://new-host:1883" {
		t.Errorf("broker: got %v, want tcp://new-host:1883", merged.Broker)
	}
}

// TestDDBOverlay_AddsNewRoute validates that an overlay route r2 is
// appended alongside the base route r1 in the merged config.
func TestDDBOverlay_AddsNewRoute(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-radd")
	ctx := context.Background()

	overlay := overlayWithRoute("test-bridge", "r2")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	routeIDs := make(map[string]bool, len(cfg.Routes))
	for _, r := range cfg.Routes {
		routeIDs[r.ID] = true
	}
	if !routeIDs["r1"] {
		t.Error("missing base route r1")
	}
	if !routeIDs["r2"] {
		t.Error("missing overlay route r2")
	}
}

// TestDDBOverlay_ReplacesRouteByID validates that an overlay route
// with the same ID as a base route replaces it entirely.
func TestDDBOverlay_ReplacesRouteByID(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-rrepl")
	ctx := context.Background()

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx-base",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"bind-base"},
			},
		},
		Stores: ports.StoresConfig{
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Routes) != 1 {
		t.Fatalf("Routes: got %d, want 1", len(cfg.Routes))
	}
	if cfg.Routes[0].DeliveryMode != "shared_outbox" {
		t.Errorf("DeliveryMode: got %q, want shared_outbox", cfg.Routes[0].DeliveryMode)
	}
}

// TestDDBOverlay_OverridesBridgeSettings validates that overlay
// non-zero bridge fields override base values while preserving
// fields the overlay does not set.
func TestDDBOverlay_OverridesBridgeSettings(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-bset")
	ctx := context.Background()

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:           "test-bridge",
			LogLevel:     "debug",
			DrainTimeout: "5s",
		},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Bridge.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want debug", cfg.Bridge.LogLevel)
	}
	if cfg.Bridge.DrainTimeout != "5s" {
		t.Errorf("DrainTimeout: got %q, want 5s", cfg.Bridge.DrainTimeout)
	}
	if cfg.Bridge.ShutdownTimeout != "5s" {
		t.Errorf("ShutdownTimeout: got %q, want 5s (preserved from base)", cfg.Bridge.ShutdownTimeout)
	}
}

// TestDDBOverlay_ReplacesConfigWatch validates that the overlay
// ConfigWatch replaces the base when non-nil.
func TestDDBOverlay_ReplacesConfigWatch(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-cw")
	ctx := context.Background()

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		ConfigWatch: &ports.ConfigWatchDef{
			Mode:         "poll",
			PollInterval: "1s",
		},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.ConfigWatch == nil {
		t.Fatal("ConfigWatch is nil")
	}
	if cfg.ConfigWatch.Mode != "poll" {
		t.Errorf("Mode: got %q, want poll", cfg.ConfigWatch.Mode)
	}
	if cfg.ConfigWatch.PollInterval != "1s" {
		t.Errorf("PollInterval: got %q, want 1s", cfg.ConfigWatch.PollInterval)
	}
}

// TestDDBOverlay_ReplacesStorePerRole validates that overlay stores
// replace per-role while leaving other roles from base intact.
func TestDDBOverlay_ReplacesStorePerRole(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-store")
	ctx := context.Background()

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Stores.Outbox == nil || cfg.Stores.Outbox.Type != "memory" {
		t.Error("expected stores.outbox.type=memory from overlay")
	}
	if cfg.Stores.Lease != nil {
		t.Error("expected stores.lease to be nil (not set by either layer)")
	}
}

// TestDDBOverlay_EmptyOverlay_PreservesBase validates that an
// overlay with only bridge.id set preserves all base values.
func TestDDBOverlay_EmptyOverlay_PreservesBase(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-empty")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Receivers) != 1 || cfg.Receivers[0].ID != "rx-base" {
		t.Errorf("expected base receiver rx-base, got %v", cfg.Receivers)
	}
	if len(cfg.Senders) != 1 || cfg.Senders[0].ID != "tx-base" {
		t.Errorf("expected base sender tx-base, got %v", cfg.Senders)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].ID != "r1" {
		t.Errorf("expected base route r1, got %v", cfg.Routes)
	}
	if cfg.Bridge.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want info (from base)", cfg.Bridge.LogLevel)
	}
}

// TestDDBOverlay_PartialOverlay_OnlyAddsNewSenders validates that
// an overlay with only a new sender appends it alongside base senders.
func TestDDBOverlay_PartialOverlay_OnlyAddsNewSenders(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "overlay-psend")
	ctx := context.Background()

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Senders: []ports.SenderDef{
			{ID: "tx-2", Transport: "fake"},
		},
	}
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	cfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	senderIDs := make(map[string]bool, len(cfg.Senders))
	for _, s := range cfg.Senders {
		senderIDs[s.ID] = true
	}
	if !senderIDs["tx-base"] {
		t.Error("missing base sender tx-base")
	}
	if !senderIDs["tx-2"] {
		t.Error("missing overlay sender tx-2")
	}
}
