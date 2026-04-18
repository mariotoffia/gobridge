package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startFailCfg() *config.BridgeConfig {
	return &config.BridgeConfig{
		Bridge:    config.BridgeSettings{ID: "b", DrainTimeout: "1s"},
		Receivers: []config.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []config.SenderDef{{ID: "tx1", Transport: "fake"}, {ID: "tx2", Transport: "fake"}},
		Bindings: []config.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "a/1"},
			{ID: "b2", SenderID: "tx2", Address: "a/2"},
		},
		Routes: []config.RouteDef{{
			ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold",
			Bindings: []string{"b1", "b2"},
		}},
	}
}

func awaitSwapSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("swap timed out")
	}
}

// TestSupervisor_InitialBuildAndStart validates that the initial config produces a running runtime.
func TestSupervisor_InitialBuildAndStart(t *testing.T) {
	s := newTestSupervisor()
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), nil)
	defer func() { cancel(); <-errCh }()
	rt := s.Runtime()
	require.NotNil(t, rt)
	require.Len(t, rt.Routes(), 1)
	assert.Equal(t, "r1", rt.Routes()[0].ID)
}

// TestSupervisor_InitialBuildFailure validates that a bad initial config makes Run return error.
func TestSupervisor_InitialBuildFailure(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Run(ctx, invalidCfg(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initial build")
}

// TestSupervisor_InitialStartFailure validates that build OK but Start failure returns error.
func TestSupervisor_InitialStartFailure(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Run(ctx, startFailCfg(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initial start")
}

// TestSupervisor_RuntimeAccessorBeforeRun validates that Runtime() is nil before Run.
func TestSupervisor_RuntimeAccessorBeforeRun(t *testing.T) {
	s := newTestSupervisor()
	assert.Nil(t, s.Runtime())
}

// TestSupervisor_OverlapSwap validates that a config change swaps runtimes via overlap.
func TestSupervisor_OverlapSwap(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
	assert.NotNil(t, s.Runtime())
}

// TestSupervisor_OverlapSwap_OldRuntimeStopsCleanly validates the old runtime stops after swap.
func TestSupervisor_OverlapSwap_OldRuntimeStopsCleanly(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
	assert.False(t, oldRt.IsRunning())
}

// TestSupervisor_OverlapSwap_NewRuntimeGetsNewRoutes validates new routes after swap.
func TestSupervisor_OverlapSwap_NewRuntimeGetsNewRoutes(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r-new"), time.Second))
	require.True(t, waitForRouteID(s, "r-new", 2*time.Second))
	routes := s.Runtime().Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "r-new", routes[0].ID)
}

// TestSupervisor_PrepareCommitSwap validates the two-phase commit build via session counts.
func TestSupervisor_PrepareCommitSwap(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
	sessions, _, _ := ef.Counts()
	assert.Greater(t, sessions, 0)
}

// TestSupervisor_PrepareCommitSwap_SessionsNotCreatedDuringPrepare validates sessions are deferred.
func TestSupervisor_PrepareCommitSwap_SessionsNotCreatedDuringPrepare(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	sessions, _, _ := ef.Counts()
	assert.Equal(t, 0, sessions, "no exclusive sessions before config change")
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
	sessions, _, _ = ef.Counts()
	assert.Equal(t, 1, sessions, "session created during Complete phase")
}

// TestSupervisor_PrepareCommitSwap_SessionsCreatedAfterOldStops validates ordering via IsRunning.
func TestSupervisor_PrepareCommitSwap_SessionsCreatedAfterOldStops(t *testing.T) {
	s, ef := newTestSupervisorWithExclusive()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	ef.SessionFn = func(_ context.Context, _ config.SessionDef) (ports.Session, error) {
		assert.False(t, oldRt.IsRunning(), "old runtime must be stopped before new session")
		return &fakeSession{}, nil
	}
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
}

// TestSupervisor_AutoDetect validates swap mode selection for various transport combinations.
func TestSupervisor_AutoDetect(t *testing.T) {
	tests := []struct {
		name      string
		exclusive bool
		opts      []SupervisorOption
		cfgFn     func() *config.BridgeConfig
		wantMode  SwapMode
	}{
		{"ExclusiveUsePrepareCommit", true, nil,
			func() *config.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
			SwapPrepareCommit},
		{"NonExclusiveUseOverlap", false, nil,
			func() *config.BridgeConfig { return quickCfg("r2") },
			SwapOverlap},
		{"MixedTransports", true, nil,
			func() *config.BridgeConfig {
				c := supervisorTestConfigWithSession("r2", "s1")
				c.Sessions = append(c.Sessions, config.SessionDef{ID: "s-plain", Transport: "fake", SessionMode: "ephemeral"})
				return c
			}, SwapPrepareCommit},
		{"ForcePrepareCommit_NoExclusive", false,
			[]SupervisorOption{WithSwapMode(SwapPrepareCommit)},
			func() *config.BridgeConfig { return quickCfg("r2") },
			SwapPrepareCommit},
		{"ForceOverlap_WithExclusive", true,
			[]SupervisorOption{WithSwapMode(SwapOverlap)},
			func() *config.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
			SwapOverlap},
		{"ReportsResolvedMode_NotAuto", true, nil,
			func() *config.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
			SwapPrepareCommit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ev SwapEvent
			done := make(chan struct{}, 1)
			opts := append(tt.opts, WithOnSwap(func(e SwapEvent) { ev = e; done <- struct{}{} }))
			var s *Supervisor
			if tt.exclusive {
				s, _ = newTestSupervisorWithExclusive(opts...)
			} else {
				s = newTestSupervisor(opts...)
			}
			ch := make(chan *config.BridgeConfig, 1)
			cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
			defer func() { cancel(); <-errCh }()
			require.True(t, sendConfig(ch, tt.cfgFn(), time.Second))
			awaitSwapSignal(t, done)
			assert.NotEqual(t, SwapAuto, ev.SwapMode)
			assert.Equal(t, tt.wantMode, ev.SwapMode)
		})
	}
}

// TestSupervisor_AutoDetect_ConfigChangeRemovesExclusive validates mode changes across swaps.
func TestSupervisor_AutoDetect_ConfigChangeRemovesExclusive(t *testing.T) {
	var mu sync.Mutex
	var events []SwapEvent
	swapped := make(chan struct{}, 2)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(func(e SwapEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		swapped <- struct{}{}
	}))
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	awaitSwapSignal(t, swapped)
	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	awaitSwapSignal(t, swapped)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	assert.Equal(t, SwapPrepareCommit, events[0].SwapMode)
	assert.Equal(t, SwapOverlap, events[1].SwapMode)
}

// TestSupervisor_MultipleConfigChanges validates 3 sequential configs each produce correct routes.
func TestSupervisor_MultipleConfigChanges(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	for _, id := range []string{"r2", "r3", "r4"} {
		require.True(t, sendConfig(ch, quickCfg(id), time.Second))
		require.True(t, waitForRouteID(s, id, 2*time.Second))
	}
}

// TestSupervisor_RapidConfigChanges_WithDirectStrategy validates all 5 rapid changes apply in order.
func TestSupervisor_RapidConfigChanges_WithDirectStrategy(t *testing.T) {
	var mu sync.Mutex
	var applied []string
	swapped := make(chan struct{}, 10)
	s := newTestSupervisor(WithOnSwap(func(e SwapEvent) {
		if e.Error == nil {
			mu.Lock()
			applied = append(applied, e.NewConfig.Routes[0].ID)
			mu.Unlock()
		}
		swapped <- struct{}{}
	}))
	ch := make(chan *config.BridgeConfig, 5)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r0"), ch)
	defer func() { cancel(); <-errCh }()
	ids := []string{"r1", "r2", "r3", "r4", "r5"}
	for _, id := range ids {
		ch <- quickCfg(id)
	}
	for i := 0; i < 5; i++ {
		awaitSwapSignal(t, swapped)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, ids, applied)
}

// TestSupervisor_RapidConfigChanges_WithDebouncedStrategy validates only the last of 5 rapid changes applies.
func TestSupervisor_RapidConfigChanges_WithDebouncedStrategy(t *testing.T) {
	var mu sync.Mutex
	var applied []string
	swapped := make(chan struct{}, 10)
	s := newTestSupervisor(
		WithReconfigStrategy(NewDebouncedStrategy(100*time.Millisecond, nil)),
		WithOnSwap(func(e SwapEvent) {
			if e.Error == nil {
				mu.Lock()
				applied = append(applied, e.NewConfig.Routes[0].ID)
				mu.Unlock()
			}
			swapped <- struct{}{}
		}),
	)
	ch := make(chan *config.BridgeConfig, 5)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r0"), ch)
	defer func() { cancel(); <-errCh }()
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		ch <- quickCfg(id)
	}
	awaitSwapSignal(t, swapped)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, applied, 1)
	assert.Equal(t, "r5", applied[0])
}

// TestSupervisor_AlternatingValidInvalid validates valid configs apply and invalid ones are rejected.
func TestSupervisor_AlternatingValidInvalid(t *testing.T) {
	var mu sync.Mutex
	var events []SwapEvent
	swapped := make(chan struct{}, 10)
	s := newTestSupervisor(WithOnSwap(func(e SwapEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		swapped <- struct{}{}
	}))
	ch := make(chan *config.BridgeConfig, 3)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	ch <- quickCfg("r2")
	ch <- invalidCfg()
	ch <- quickCfg("r3")
	for i := 0; i < 3; i++ {
		awaitSwapSignal(t, swapped)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 3)
	assert.NoError(t, events[0].Error)
	assert.Error(t, events[1].Error)
	assert.NoError(t, events[2].Error)
}

// TestSupervisor_ConfigRollback_AfterFailure validates A→C transition when B fails.
func TestSupervisor_ConfigRollback_AfterFailure(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 2)
	cancel, errCh := quickSupervisorRun(s, quickCfg("rA"), ch)
	defer func() { cancel(); <-errCh }()
	ch <- invalidCfg()
	ch <- quickCfg("rC")
	require.True(t, waitForRouteID(s, "rC", 3*time.Second))
	assert.Equal(t, "rC", s.Runtime().Routes()[0].ID)
}

// TestSupervisor_SwapCallback_Success validates SwapEvent fields on a successful swap.
func TestSupervisor_SwapCallback_Success(t *testing.T) {
	var ev SwapEvent
	done := make(chan struct{}, 1)
	s := newTestSupervisor(WithOnSwap(func(e SwapEvent) { ev = e; done <- struct{}{} }))
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	awaitSwapSignal(t, done)
	assert.NoError(t, ev.Error)
	assert.Equal(t, "r1", ev.OldConfig.Routes[0].ID)
	assert.Equal(t, "r2", ev.NewConfig.Routes[0].ID)
	assert.Greater(t, ev.Duration, time.Duration(0))
	assert.Equal(t, SwapOverlap, ev.SwapMode)
}

// TestSupervisor_SwapCallback_BuildFailure validates SwapEvent fields when build fails.
func TestSupervisor_SwapCallback_BuildFailure(t *testing.T) {
	var ev SwapEvent
	done := make(chan struct{}, 1)
	s := newTestSupervisor(WithOnSwap(func(e SwapEvent) { ev = e; done <- struct{}{} }))
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	bad := invalidCfg()
	require.True(t, sendConfig(ch, bad, time.Second))
	awaitSwapSignal(t, done)
	assert.Error(t, ev.Error)
	assert.Equal(t, "r1", ev.OldConfig.Routes[0].ID)
	assert.Equal(t, bad, ev.NewConfig)
}

// TestSupervisor_NoSwapCallback_WhenNoneSet validates no panic with nil onSwap.
func TestSupervisor_NoSwapCallback_WhenNoneSet(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
}

// TestSupervisor_ContextCancellation validates that cancelling ctx stops Run and the runtime.
func TestSupervisor_ContextCancellation(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), nil)
	rt := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt)
	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	assert.False(t, rt.IsRunning())
}

// TestSupervisor_ChannelClosed_GracefulShutdown validates Run returns nil on channel close.
func TestSupervisor_ChannelClosed_GracefulShutdown(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *config.BridgeConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	rt := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt)
	close(ch)
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after channel close")
	}
	assert.False(t, rt.IsRunning())
}

// TestSupervisor_NilChangesChannel validates the runtime runs until ctx cancel with nil channel.
func TestSupervisor_NilChangesChannel(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), nil)
	rt := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt)
	assert.True(t, rt.IsRunning())
	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestSupervisor_EmptyConfig_Rejected validates that BridgeConfig{} fails validation.
func TestSupervisor_EmptyConfig_Rejected(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Run(ctx, &config.BridgeConfig{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.id is required")
}
