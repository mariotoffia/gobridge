package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// ===============================================================
// Group 4: Erroneous Config & Rollback
//
// Validates that invalid or failing configs are handled gracefully:
//   - Invalid merged configs are dropped by config.Manager
//   - Build failures trigger supervisor rollback to old config
//   - Multiple failures leave the old config intact
// ===============================================================

// TestDDBRollback_InvalidOverlay_ManagerDrops validates that an
// overlay producing an invalid merged config is dropped by the
// Manager, leaving the supervisor running the old config.
//
// Scenario:
// -----------------------------------------------
//
//	r1 (running) → invalid overlay (dropped) → r1 still runs
//
// -----------------------------------------------
func TestDDBRollback_InvalidOverlay_ManagerDrops(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "rb-invalid")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	var swapCount atomic.Int32
	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(_ bridge.SwapEvent) { swapCount.Add(1) }),
	)

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, errCh, 5*time.Second)

	// Save invalid overlay: route references nonexistent receiver.
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

	time.Sleep(500 * time.Millisecond) // NEGATIVE: verify Manager drops invalid config (no swap event)

	if count := swapCount.Load(); count != 0 {
		t.Errorf("swap count: got %d, want 0 (Manager should drop)", count)
	}

	// Verify r1 is still running.
	rt := s.Runtime()
	if rt == nil {
		t.Fatal("runtime is nil")
	}
	routes := rt.Routes()
	if len(routes) == 0 || routes[0].ID != "r1" {
		t.Errorf("expected route r1, got %v", routes)
	}
}

// TestDDBRollback_ValidOverlayButBuildFails_KeepsOldConfig validates
// that when a valid overlay passes validation but the builder fails
// (broken transport), the supervisor keeps the old config.
//
// Scenario:
// -----------------------------------------------
//
//	r1 (running) → r-broken (build fails) → r1 restored
//
// -----------------------------------------------
func TestDDBRollback_ValidOverlayButBuildFails_KeepsOldConfig(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "rb-buildfail")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 4)
	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	// Register "broken" transport that always fails.
	s.RegisterTransport("broken", &cfgBrokenTransportFactory{
		err: fmt.Errorf("simulated transport failure"),
	})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, errCh, 5*time.Second)

	// Save overlay that adds a route using "broken" transport.
	// This is structurally valid (passes config.Validate) but builder fails.
	brokenOverlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-broken", Transport: "broken"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-broken", Transport: "broken"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-broken", SenderID: "tx-broken", Address: "addr/broken"},
		},
		Routes: []ports.RouteDef{
			{ID: "r-broken", ReceiverID: "rx-broken", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-broken"}},
		},
	}
	if err := loader.Save(ctx, brokenOverlay); err != nil {
		t.Fatalf("save broken: %v", err)
	}

	// Wait for swap event with error.
	select {
	case ev := <-eventCh:
		if ev.Error == nil {
			t.Fatal("expected swap error for broken transport")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}

	// Old config (r1) should still be active.
	cfg := s.Config()
	if cfg == nil {
		t.Fatal("config is nil after rollback")
	}
	hasR1 := false
	for _, r := range cfg.Routes {
		if r.ID == "r1" {
			hasR1 = true
		}
	}
	if !hasR1 {
		t.Error("old config route r1 missing after rollback")
	}
}

// TestDDBRollback_ValidOverlayButStartFails_RecoversOldConfig validates
// that when the builder succeeds but starts a different transport that
// returns an error, the supervisor recovers the old config.
func TestDDBRollback_ValidOverlayButStartFails_RecoversOldConfig(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "rb-startfail")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 4)
	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	s.RegisterTransport("broken", &cfgBrokenTransportFactory{
		err: fmt.Errorf("receiver creation failed"),
	})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, errCh, 5*time.Second)

	// Overlay adds route with broken transport (receiver fails on NewReceiver).
	brokenOverlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-broken", Transport: "broken"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-broken", Transport: "broken"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-broken", SenderID: "tx-broken", Address: "addr/broken"},
		},
		Routes: []ports.RouteDef{
			{ID: "r-broken", ReceiverID: "rx-broken", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-broken"}},
		},
	}
	if err := loader.Save(ctx, brokenOverlay); err != nil {
		t.Fatalf("save broken: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error == nil {
			t.Fatal("expected swap error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}

	// Supervisor should have recovered with old config.
	rt := s.Runtime()
	if rt == nil {
		t.Fatal("runtime is nil after recovery")
	}
}

// TestDDBRollback_MultipleFailures_OldConfigSurvives validates that
// multiple consecutive failures do not corrupt the running config.
//
// Scenario:
// -----------------------------------------------
//
//	r1 → invalid (dropped) → broken (build fail, rollback)
//	   → valid r3 (success)
//
// -----------------------------------------------
func TestDDBRollback_MultipleFailures_OldConfigSurvives(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "rb-multi")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 10)
	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)
	s.RegisterTransport("broken", &cfgBrokenTransportFactory{
		err: fmt.Errorf("broken transport"),
	})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, errCh, 5*time.Second)

	// Step 1: Invalid overlay (dropped by Manager).
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
	time.Sleep(300 * time.Millisecond) // SYNC: let config watcher detect invalid overlay

	// Step 2: Broken overlay (build fails, rollback).
	brokenOverlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx-broken", Transport: "broken"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx-broken", Transport: "broken"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-broken", SenderID: "tx-broken", Address: "addr/broken"},
		},
		Routes: []ports.RouteDef{
			{ID: "r-broken", ReceiverID: "rx-broken", DeliveryMode: "direct_hold",
				Bindings: []string{"bind-broken"}},
		},
	}
	if err := loader.Save(ctx, brokenOverlay); err != nil {
		t.Fatalf("save broken: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error == nil {
			t.Fatal("expected error for broken overlay")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for broken swap event")
	}

	// Verify r1 still active.
	if rt := s.Runtime(); rt != nil {
		routes := rt.Routes()
		if len(routes) == 0 || routes[0].ID != "r1" {
			t.Errorf("after broken: expected r1, got %v", routes)
		}
	}

	// Step 3: Valid overlay with route r3.
	validOverlay := overlayWithRoute("test-bridge", "r3")
	if err := loader.Save(ctx, validOverlay); err != nil {
		t.Fatalf("save valid: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("unexpected swap error for valid overlay: %v", ev.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for valid swap event")
	}

	waitForSupervisorRouteID(t, s, "r3", 5*time.Second)
}

// TestDDBRollback_InvalidThenValid_RecoversFully validates that after
// an invalid overlay is dropped, a subsequent valid overlay succeeds.
func TestDDBRollback_InvalidThenValid_RecoversFully(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "rb-invval")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save v1: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watchCh, err := mgr.Watch(watchCtx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	eventCh := make(chan bridge.SwapEvent, 10)
	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, errCh, 5*time.Second)

	// Invalid overlay (dropped by Manager).
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
	time.Sleep(300 * time.Millisecond) // SYNC: let config watcher detect invalid overlay before saving valid one

	// Valid overlay adding r2.
	validOverlay := overlayWithRoute("test-bridge", "r2")
	if err := loader.Save(ctx, validOverlay); err != nil {
		t.Fatalf("save valid: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("unexpected error: %v", ev.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}

	waitForSupervisorRouteID(t, s, "r2", 5*time.Second)
}
