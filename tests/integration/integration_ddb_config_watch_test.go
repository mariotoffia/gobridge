package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// ===============================================================
// Group 2: Config Change Detection & Propagation
//
// Validates that config.Manager.Watch() correctly detects DynamoDB
// version changes, re-merges all layers, validates the merged
// result, and emits (or drops) merged configs.
// ===============================================================

// TestDDBWatch_VersionChangeTriggersEmission validates that a DynamoDB
// version change triggers the Manager to emit a merged config.
func TestDDBWatch_VersionChangeTriggersEmission(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-emit")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	overlay2 := minimalOverlay("test-bridge")
	overlay2.Bridge.LogLevel = "debug"
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	got := waitForConfig(t, ch, 5*time.Second)
	if got.Bridge.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want debug", got.Bridge.LogLevel)
	}
}

// TestDDBWatch_NoVersionChange_NoEmission validates that no config
// is emitted when the DynamoDB version remains unchanged.
func TestDDBWatch_NoVersionChange_NoEmission(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-noemit")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// No new save; wait for 5 poll cycles (500ms at 100ms interval).
	waitForNoConfig(t, ch, 500*time.Millisecond)
}

// TestDDBWatch_ManagerRebuildsMergedConfig validates that the emitted
// config contains fields from both the file base and the new DDB overlay.
func TestDDBWatch_ManagerRebuildsMergedConfig(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-merge")
	ctx := context.Background()

	overlay := overlayWithRoute("test-bridge", "r2")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Change overlay to add r3 instead, triggering a version bump.
	overlay2 := overlayWithRoute("test-bridge", "r3")
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	got := waitForConfig(t, ch, 5*time.Second)

	// Merged config must have r1 (from base) and r3 (from new overlay).
	routeIDs := make(map[string]bool, len(got.Routes))
	for _, r := range got.Routes {
		routeIDs[r.ID] = true
	}
	if !routeIDs["r1"] {
		t.Error("missing base route r1 in merged config")
	}
	if !routeIDs["r3"] {
		t.Error("missing overlay route r3 in merged config")
	}
}

// TestDDBWatch_InvalidMergedConfig_DroppedByManager validates that
// an overlay producing an invalid merged config is silently dropped.
//
// Scenario:
// -----------------------------------------------
//
//	Base: route r1 with receiver rx-base
//	DDB v1: minimal (valid)
//	DDB v2: route r-bad references rx-missing
//	Merged: validation fails (rx-missing not found)
//	Result: no emission on watch channel
//
// -----------------------------------------------
func TestDDBWatch_InvalidMergedConfig_DroppedByManager(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-invalid")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Overlay that causes validation failure: route references missing receiver.
	invalidOverlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Routes: []ports.RouteDef{
			{
				ID:           "r-bad",
				ReceiverID:   "rx-missing",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"bind-base"},
			},
		},
	}
	if err := loader.Save(ctx, invalidOverlay); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	waitForNoConfig(t, ch, 500*time.Millisecond)
}

// TestDDBWatch_ManagerStop_ClosesChannel validates that calling
// Manager.Stop() closes the watch channel.
func TestDDBWatch_ManagerStop_ClosesChannel(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-stop")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	ch, err := mgr.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	mgr.Stop()

	select {
	case _, ok := <-ch:
		if ok {
			// May receive a final config before close; drain it.
			_, ok = <-ch
		}
		if ok {
			t.Error("expected channel to be closed after Stop()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close after Stop()")
	}
}

// TestDDBWatch_ContextCancel_ClosesChannel validates that cancelling
// the watch context closes the channel.
func TestDDBWatch_ContextCancel_ClosesChannel(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-ctxcancel")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			_, ok = <-ch
		}
		if ok {
			t.Error("expected channel to be closed after context cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close after cancel")
	}
}

// TestDDBWatch_MultipleOverlayChanges_EachEmits validates that
// sequential DDB saves each produce a merged config emission.
func TestDDBWatch_MultipleOverlayChanges_EachEmits(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-multi")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Save v2 with log_level=debug.
	v2 := minimalOverlay("test-bridge")
	v2.Bridge.LogLevel = "debug"
	if err := loader.Save(ctx, v2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	got := waitForConfig(t, ch, 5*time.Second)
	if got.Bridge.LogLevel != "debug" {
		t.Errorf("v2 LogLevel: got %q, want debug", got.Bridge.LogLevel)
	}

	// Save v3 with log_level=warn.
	v3 := minimalOverlay("test-bridge")
	v3.Bridge.LogLevel = "warn"
	if err := loader.Save(ctx, v3); err != nil {
		t.Fatalf("save v3: %v", err)
	}

	got = waitForConfig(t, ch, 5*time.Second)
	if got.Bridge.LogLevel != "warn" {
		t.Errorf("v3 LogLevel: got %q, want warn", got.Bridge.LogLevel)
	}
}

// TestDDBWatch_RapidSaves_AtLeastOneEmission validates that rapid
// sequential DDB saves result in at least one config emission.
func TestDDBWatch_RapidSaves_AtLeastOneEmission(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-rapid")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	levels := []string{"debug", "warn", "error"}
	for _, lvl := range levels {
		v := minimalOverlay("test-bridge")
		v.Bridge.LogLevel = lvl
		if err := loader.Save(ctx, v); err != nil {
			t.Fatalf("save %s: %v", lvl, err)
		}
	}

	got := waitForConfig(t, ch, 5*time.Second)
	validLevels := map[string]bool{"debug": true, "warn": true, "error": true}
	if !validLevels[got.Bridge.LogLevel] {
		t.Errorf("LogLevel: got %q, want one of debug/warn/error", got.Bridge.LogLevel)
	}
}

// TestDDBWatch_InvalidThenValid_OnlyValidEmits validates that an
// invalid overlay is dropped and a subsequent valid overlay emits.
func TestDDBWatch_InvalidThenValid_OnlyValidEmits(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "watch-invval")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	if _, err := mgr.Load(ctx); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Save invalid overlay (route references missing receiver).
	invalidOverlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Routes: []ports.RouteDef{
			{ID: "r-bad", ReceiverID: "rx-missing", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-base"}},
		},
	}
	if err := loader.Save(ctx, invalidOverlay); err != nil {
		t.Fatalf("save invalid: %v", err)
	}

	time.Sleep(300 * time.Millisecond) // SYNC: let config watcher poll pick up invalid version

	// Save valid overlay with log_level change.
	validOverlay := minimalOverlay("test-bridge")
	validOverlay.Bridge.LogLevel = "error"
	if err := loader.Save(ctx, validOverlay); err != nil {
		t.Fatalf("save valid: %v", err)
	}

	got := waitForConfig(t, ch, 5*time.Second)
	if got.Bridge.LogLevel != "error" {
		t.Errorf("LogLevel: got %q, want error", got.Bridge.LogLevel)
	}
}
