package integration_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
)

// ===============================================================
// Group 3: Supervisor Config Swap with Fake Transports
//
// Validates the full pipeline: DynamoDB config change →
// config.Manager.Watch → bridge.Supervisor.apply → runtime swap.
// Uses fake transports (no Docker containers besides DynamoDB).
// ===============================================================

// TestDDBSupervisor_InitialLoadAndRun validates that the supervisor
// starts a runtime with the expected routes from the merged config.
func TestDDBSupervisor_InitialLoadAndRun(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-init")
	ctx := context.Background()

	overlay := minimalOverlay("test-bridge")
	if err := loader.Save(ctx, overlay); err != nil {
		t.Fatalf("save overlay: %v", err)
	}

	mgr := newTestConfigManager(t, basePath, loader)
	initialCfg, err := mgr.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	s := newCfgTestSupervisor(bridge.WithSupervisorLogger(slog.Default()))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	_, errCh := runSupervisorInBackground(runCtx, s, initialCfg, nil)
	defer func() { cancel(); <-errCh }()

	rt := waitForSupervisorRuntime(t, s, 5*time.Second)
	routes := rt.Routes()
	if len(routes) == 0 {
		t.Fatal("expected at least one route")
	}
	if routes[0].ID != "r1" {
		t.Errorf("route ID: got %q, want r1", routes[0].ID)
	}
}

// TestDDBSupervisor_OverlayChangeSwapsRuntime validates that a DDB
// overlay change triggers the supervisor to swap runtimes, with the
// new runtime containing the added route.
func TestDDBSupervisor_OverlayChangeSwapsRuntime(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-swap")
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

	s := newCfgTestSupervisor(
		bridge.WithSupervisorLogger(slog.Default()),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
	)

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, 5*time.Second)

	// Save overlay that adds route r2.
	overlay2 := overlayWithRoute("test-bridge", "r2")
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	waitForSupervisorRouteID(t, s, "r2", 5*time.Second)
}

// TestDDBSupervisor_SwapEvent_ReportsCorrectConfigs validates that
// the OnSwap callback receives correct old and new config references.
func TestDDBSupervisor_SwapEvent_ReportsCorrectConfigs(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-event")
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

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, 5*time.Second)

	overlay2 := overlayWithRoute("test-bridge", "r2")
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("swap error: %v", ev.Error)
		}
		// Old config should have route r1 only.
		if len(ev.OldConfig.Routes) == 0 || ev.OldConfig.Routes[0].ID != "r1" {
			t.Errorf("OldConfig first route: got %v, want r1", ev.OldConfig.Routes)
		}
		// New config should have route r2.
		hasR2 := false
		for _, r := range ev.NewConfig.Routes {
			if r.ID == "r2" {
				hasR2 = true
			}
		}
		if !hasR2 {
			t.Error("NewConfig missing route r2")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}
}

// TestDDBSupervisor_SwapEvent_ReportsMode validates that the
// SwapEvent reports the correct swap mode.
func TestDDBSupervisor_SwapEvent_ReportsMode(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-mode")
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
		bridge.WithSwapMode(bridge.SwapOverlap),
		bridge.WithReconfigStrategy(bridge.NewDirectStrategy()),
		bridge.WithOnSwap(func(ev bridge.SwapEvent) { eventCh <- ev }),
	)

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, 5*time.Second)

	overlay2 := minimalOverlay("test-bridge")
	overlay2.Bridge.LogLevel = "debug"
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.SwapMode != bridge.SwapOverlap {
			t.Errorf("SwapMode: got %v, want SwapOverlap", ev.SwapMode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}
}

// TestDDBSupervisor_DebouncedStrategy_CoalescesChanges validates
// that rapid DDB saves are coalesced into a single swap when using
// a debounced strategy.
func TestDDBSupervisor_DebouncedStrategy_CoalescesChanges(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-debounce")
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
		bridge.WithReconfigStrategy(bridge.NewDebouncedStrategy(300*time.Millisecond, nil)),
		bridge.WithOnSwap(func(_ bridge.SwapEvent) { swapCount.Add(1) }),
	)

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, 5*time.Second)

	// Rapid saves.
	for _, lvl := range []string{"debug", "warn", "error"} {
		v := minimalOverlay("test-bridge")
		v.Bridge.LogLevel = lvl
		if err := loader.Save(ctx, v); err != nil {
			t.Fatalf("save %s: %v", lvl, err)
		}
	}

	time.Sleep(1 * time.Second) // SYNC: wait for debounce quiet period + poll interval + build time

	count := swapCount.Load()
	if count != 1 {
		t.Errorf("swap count: got %d, want 1 (debounced)", count)
	}
}

// TestDDBSupervisor_SequentialChanges_EachApplied validates that
// sequential DDB saves with pauses between them each trigger a swap.
func TestDDBSupervisor_SequentialChanges_EachApplied(t *testing.T) {
	basePath := writeBaseYAML(t, "test-bridge")
	loader := ddbConfigLoader(t, "sup-seq")
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

	waitForSupervisorRuntime(t, s, 5*time.Second)

	// Sequential changes with pause between each to allow poll detection.
	for _, lvl := range []string{"debug", "warn"} {
		v := minimalOverlay("test-bridge")
		v.Bridge.LogLevel = lvl
		if err := loader.Save(ctx, v); err != nil {
			t.Fatalf("save %s: %v", lvl, err)
		}
		// Wait for swap to complete.
		select {
		case ev := <-eventCh:
			if ev.Error != nil {
				t.Errorf("swap error for %s: %v", lvl, ev.Error)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for swap for %s", lvl)
		}
	}
}

// TestDDBSupervisor_PrepareCommitMode_WithExclusiveSession validates
// that the supervisor auto-detects SwapPrepareCommit when an
// exclusive transport is registered.
func TestDDBSupervisor_PrepareCommitMode_WithExclusiveSession(t *testing.T) {
	basePath := writeBaseYAMLWithSession(t, "test-bridge", "exc-sess", "exclusive")
	loader := ddbConfigLoader(t, "sup-pc")
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
	// Register the exclusive transport that triggers PrepareCommit.
	s.RegisterTransport("exclusive", &cfgExclusiveTransportFactory{})

	cancel, errCh := runSupervisorInBackground(watchCtx, s, initialCfg, watchCh)
	defer func() { cancel(); watchCancel(); <-errCh }()

	waitForSupervisorRuntime(t, s, 5*time.Second)

	// Trigger a swap with log level change.
	overlay2 := minimalOverlay("test-bridge")
	overlay2.Bridge.LogLevel = "debug"
	if err := loader.Save(ctx, overlay2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	select {
	case ev := <-eventCh:
		if ev.Error != nil {
			t.Fatalf("swap error: %v", ev.Error)
		}
		if ev.SwapMode != bridge.SwapPrepareCommit {
			t.Errorf("SwapMode: got %v, want SwapPrepareCommit", ev.SwapMode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}
}
