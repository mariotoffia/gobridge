package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func startFailCfg() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "b", DrainTimeout: "1s"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx1", Transport: "fake"}, {ID: "tx2", Transport: "fake"}},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "a/1"},
			{ID: "b2", SenderID: "tx2", Address: "a/2"},
		},
		Routes: []ports.RouteDef{{
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

type clockAdvancingTransportFactory struct {
	fakeTransportFactory
	clk           *clocktest.Fake
	advanceOnCall int
	advanceBy     time.Duration
	calls         int
}

func (f *clockAdvancingTransportFactory) NewReceiver(ctx context.Context, spec ports.ReceiverSpec, sess ports.Session) (ports.Receiver, error) {
	f.calls++
	if f.calls == f.advanceOnCall {
		f.clk.Advance(f.advanceBy)
	}
	return f.fakeTransportFactory.NewReceiver(ctx, spec, sess)
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

// TestSupervisor_StaticallyInvalidConfigFailsAtBuild validates finding 5 / C2:
// a statically-rejectable initial config (direct_hold with multiple bindings and
// no resolver) is rejected during the BUILD phase — complete() runs the
// runtime's ValidateRoutes before Start — so the supervisor reports it as
// "initial build" and never produces a runtime. Route validation must NOT be
// deferred to Start, where (during a hot swap) the old runtime would already be
// stopped.
func TestSupervisor_StaticallyInvalidConfigFailsAtBuild(t *testing.T) {
	s := newTestSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Run(ctx, startFailCfg(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initial build",
		"a statically-rejectable config must fail at build (before Start), not at start")
	assert.Nil(t, s.Runtime(), "no runtime should be produced for an invalid config")
}

// TestSupervisor_RuntimeAccessorBeforeRun validates that Runtime() is nil before Run.
func TestSupervisor_RuntimeAccessorBeforeRun(t *testing.T) {
	s := newTestSupervisor()
	assert.Nil(t, s.Runtime())
}

// TestSupervisor_OverlapSwap validates that a config change swaps runtimes via overlap.
func TestSupervisor_OverlapSwap(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.True(t, waitForRouteID(s, "r2", 2*time.Second))
	assert.NotNil(t, s.Runtime())
}

// TestSupervisor_OverlapSwap_OldRuntimeStopsCleanly validates the old runtime stops after swap.
func TestSupervisor_OverlapSwap_OldRuntimeStopsCleanly(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
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
		cfgFn     func() *ports.BridgeConfig
		wantMode  SwapMode
	}{
		{"ExclusiveUsePrepareCommit", true, nil,
			func() *ports.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
			SwapPrepareCommit},
		{"NonExclusiveUseOverlap", false, nil,
			func() *ports.BridgeConfig { return quickCfg("r2") },
			SwapOverlap},
		{"MixedTransports", true, nil,
			func() *ports.BridgeConfig {
				c := supervisorTestConfigWithSession("r2", "s1")
				c.Sessions = append(c.Sessions, ports.SessionDef{ID: "s-plain", Transport: "fake", SessionMode: "ephemeral"})
				return c
			}, SwapPrepareCommit},
		{"ForcePrepareCommit_NoExclusive", false,
			[]SupervisorOption{WithSwapMode(SwapPrepareCommit)},
			func() *ports.BridgeConfig { return quickCfg("r2") },
			SwapPrepareCommit},
		{"ForceOverlap_WithExclusive", true,
			[]SupervisorOption{WithSwapMode(SwapOverlap)},
			func() *ports.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
			SwapOverlap},
		{"ReportsResolvedMode_NotAuto", true, nil,
			func() *ports.BridgeConfig { return supervisorTestConfigWithSession("r2", "s1") },
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
			ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 5)
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
	ch := make(chan *ports.BridgeConfig, 5)
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
	ch := make(chan *ports.BridgeConfig, 3)
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
	ch := make(chan *ports.BridgeConfig, 2)
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
	ch := make(chan *ports.BridgeConfig, 1)
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

func TestSupervisor_SwapCallback_UsesInjectedClockForDuration(t *testing.T) {
	fakeClock := clocktest.NewAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	const swapDuration = 42 * time.Second

	var ev SwapEvent
	done := make(chan struct{}, 1)
	s := newTestSupervisorTransport(
		&clockAdvancingTransportFactory{
			clk:           fakeClock,
			advanceOnCall: 2,
			advanceBy:     swapDuration,
		},
		WithSupervisorClock(fakeClock),
		WithOnSwap(func(e SwapEvent) { ev = e; done <- struct{}{} }),
	)

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer func() { cancel(); <-errCh }()
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	awaitSwapSignal(t, done)

	assert.NoError(t, ev.Error)
	assert.Equal(t, swapDuration, ev.Duration)
}

// TestSupervisor_SwapCallback_BuildFailure validates SwapEvent fields when build fails.
func TestSupervisor_SwapCallback_BuildFailure(t *testing.T) {
	var ev SwapEvent
	done := make(chan struct{}, 1)
	s := newTestSupervisor(WithOnSwap(func(e SwapEvent) { ev = e; done <- struct{}{} }))
	ch := make(chan *ports.BridgeConfig, 1)
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
	ch := make(chan *ports.BridgeConfig, 1)
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

// TestSupervisor_ChannelClosed_GracefulShutdown validates that a closed config
// change stream does NOT stop a healthy runtime (Finding 1): the supervisor
// keeps serving, marks itself degraded, and Run returns only when ctx is
// cancelled. A closed channel used to drain+stop the bridge and exit 0, turning
// a watcher failure (e.g. inotify exhaustion) into a silent total outage.
func TestSupervisor_ChannelClosed_KeepsServingAndDegrades(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	rt := waitForRuntime(s, 2*time.Second)
	require.NotNil(t, rt)

	close(ch)

	// The runtime must keep serving and the supervisor must report degraded
	// (not terminal) after the change stream closes.
	require.Eventually(t, func() bool {
		degraded, _ := s.Degraded()
		return degraded
	}, 2*time.Second, 10*time.Millisecond, "supervisor must mark degraded on channel close")
	assert.True(t, rt.IsRunning(), "runtime must keep serving after channel close")
	assert.False(t, s.Terminal(), "channel close is degraded, not terminal")

	// Run must NOT have returned yet — a closed channel is not a shutdown.
	select {
	case err := <-errCh:
		t.Fatalf("Run returned on channel close but must keep serving: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Only ctx cancellation stops the runtime and returns Run.
	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
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
	err := s.Run(ctx, &ports.BridgeConfig{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.id is required")
}

// TestSupervisor_SwapUpdatesObservableConfigVersion validates that the
// running config version is observable via Supervisor.Config after a
// successful swap, and stays at the old version after a failed swap. It also
// pins the swap-log field semantics operators alert on (finding A6): a
// failed swap logs config_version = the still-running version, not the
// rejected one, so a wedged instance is distinguishable from a healthy one.
// GoBridge observes the running version, it does not coordinate versions
// across the cluster.
func TestSupervisor_SwapUpdatesObservableConfigVersion(t *testing.T) {
	onSwap, swaps := swapChan(2)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newTestSupervisor(WithOnSwap(onSwap), WithSupervisorLogger(logger))
	ch := make(chan *ports.BridgeConfig, 1)

	initial := quickCfg("r1")
	initial.Version = 7
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()
	require.Equal(t, 7, s.Config().Version)

	// Successful swap to a higher version updates the observable version,
	// and the SwapEvent carries it for OnSwap callbacks.
	next := quickCfg("r2")
	next.Version = 8
	require.True(t, sendConfig(ch, next, time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	assert.Equal(t, 8, s.Config().Version)
	assert.Equal(t, 8, ev.NewConfig.Version)

	// The success log records config_version = the now-running version (8)
	// and the prior version as old_config_version (7).
	okRec := lastLogRecord(t, &logBuf, "supervisor: reconfiguration complete")
	assert.Equal(t, float64(8), okRec["config_version"])
	assert.Equal(t, float64(7), okRec["old_config_version"])

	// A failed swap (unresolvable transport, fails at build) must not advance
	// the observable version — the old config keeps running.
	bad := quickCfg("r3")
	bad.Version = 9
	bad.Receivers[0].Transport = "nonexistent"
	require.True(t, sendConfig(ch, bad, time.Second))
	ev = awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Equal(t, 8, s.Config().Version)

	// The failure log records config_version = the still-running version (8,
	// NOT the rejected 9) and the rejected version as attempted_config_version.
	// Logging the failed version as config_version would make a wedged instance
	// emit the same (config_version, *) pair as a healthy one, defeating the
	// divergence alert the docs tell operators to build (finding A6).
	failRec := lastLogRecord(t, &logBuf, "supervisor: reconfiguration failed")
	assert.Equal(t, float64(8), failRec["config_version"])
	assert.Equal(t, float64(9), failRec["attempted_config_version"])
}

// lastLogRecord returns the last JSON log line in buf whose "msg" equals want,
// decoded into a map. It fails the test if no such line exists.
func lastLogRecord(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		if rec["msg"] == want {
			found = rec
		}
	}
	require.NotNilf(t, found, "no log record with msg=%q", want)
	return found
}

// clusteredCfg returns a minimal valid CLUSTERED BridgeConfig
// (deployment_mode: clustered) with a unique route ID. It is the clustered
// counterpart of quickCfg used by the H8 fail-closed reload guard tests.
func clusteredCfg(id string) *ports.BridgeConfig {
	cfg := supervisorTestConfig(id)
	cfg.Bridge.DeploymentMode = "clustered"
	return cfg
}

// TestSupervisorClusteredReload proves the H8 fail-closed guard: a per-process
// live reload of (or into) a CLUSTERED deployment is refused, keeping the
// current runtime and applied config/version untouched and firing the existing
// failed-swap event + failure metric. WithAllowDestructiveReload must NOT bypass
// it (it discards local durable backlog and cannot substitute for cluster
// consensus). A genuine no-op re-emit is detected before the guard and stays
// accepted.
func TestSupervisorClusteredReload(t *testing.T) {
	t.Run("current clustered deployment refuses a non-no-op live reload", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, clusteredCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)
		oldCfg := s.Config()
		require.NotNil(t, oldCfg)

		// A real topology change (new route + a distinct version) into a still
		// clustered deployment.
		proposed := clusteredCfg("r2")
		proposed.Version = 99
		require.True(t, sendConfig(ch, proposed, time.Second))

		ev := awaitSwap(t, swaps)
		require.Error(t, ev.Error, "a clustered live reload must fail closed")
		assert.False(t, ev.Deferred, "a cluster-guard refusal is a definitive failure, not a deferred event")
		assert.Contains(t, ev.Error.Error(), "clustered")

		// Runtime (running instance), applied config, and version are all
		// unchanged: the guard fires before any build/stop.
		assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
		assert.True(t, oldRt.IsRunning(), "the current runtime must keep serving")
		assert.Equal(t, oldCfg, s.Config(), "the applied config/reference must be unchanged")
		assert.Equal(t, 0, s.Config().Version, "the applied config version must not advance")

		assert.True(t, counterHasState(rec.FindEntries(shared.MetricConfigReloads), "failure"),
			"a refused clustered reload must export the existing failure-tagged counter")
	})

	t.Run("proposed clustered deployment refuses a live reload from standalone", func(t *testing.T) {
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)
		oldCfg := s.Config()

		// Entering a clustered cohort via live reload is equally unsafe.
		require.True(t, sendConfig(ch, clusteredCfg("r2"), time.Second))

		ev := awaitSwap(t, swaps)
		require.Error(t, ev.Error, "reloading a standalone runtime INTO a clustered config must fail closed")
		assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
		assert.Equal(t, oldCfg, s.Config(), "the applied config must be unchanged")
	})

	t.Run("WithAllowDestructiveReload does not bypass the cluster guard", func(t *testing.T) {
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithOnSwap(onSwap), WithAllowDestructiveReload(true))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, clusteredCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)

		require.True(t, sendConfig(ch, clusteredCfg("r2"), time.Second))

		ev := awaitSwap(t, swaps)
		require.Error(t, ev.Error,
			"the destructive-reload escape hatch must NOT force a clustered live reload")
		assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
	})

	t.Run("version-only change to a clustered deployment is still refused", func(t *testing.T) {
		// Finding 7 boundary: the project treats BridgeConfig.Version as part of
		// content identity (config.configFingerprint json-encodes it), so a
		// version-only bump is NOT a no-op — it is a real reconfiguration and, on
		// a clustered deployment, must still fail closed.
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, clusteredCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)

		bumped := clusteredCfg("r1")
		bumped.Version = 7 // identical topology, higher version only
		require.True(t, sendConfig(ch, bumped, time.Second))

		ev := awaitSwap(t, swaps)
		require.Error(t, ev.Error, "a version-only change is content, not a no-op, so a clustered reload must be refused")
		assert.Contains(t, ev.Error.Error(), "clustered")
		assert.Same(t, oldRt, s.Runtime(), "the running runtime instance must be untouched")
		assert.Equal(t, 0, s.Config().Version, "the applied config version must not advance")
	})

	t.Run("no-op clustered reload stays accepted without a swap", func(t *testing.T) {
		// Findings 1 + 6: a byte-identical re-emit is detected BEFORE the guard and
		// acknowledged WITHOUT a swap — the running runtime, applied config, and
		// version are all preserved (no rebuild/stop/replace).
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, clusteredCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)
		oldCfg := s.Config()
		require.NotNil(t, oldCfg)

		require.True(t, sendConfig(ch, clusteredCfg("r1"), time.Second))

		ev := awaitSwap(t, swaps)
		require.NoError(t, ev.Error, "a no-op clustered reload must remain accepted")

		// No swap happened: the runtime is the SAME instance, still running, and
		// the applied config/version/reference is unchanged.
		assert.Same(t, oldRt, s.Runtime(), "a no-op must not rebuild or replace the runtime")
		assert.True(t, oldRt.IsRunning(), "the current runtime must keep serving")
		assert.Equal(t, oldCfg, s.Config(), "a no-op must leave the applied config/reference unchanged")
		assert.Equal(t, 0, s.Config().Version, "a no-op must not advance the applied version")
	})

	t.Run("a genuine route change on a standalone deployment still swaps", func(t *testing.T) {
		// Guardrail for finding 7: the canonical no-op comparison must NOT classify
		// a real route change as a no-op — single-process live reload is unchanged.
		onSwap, swaps := swapChan(1)
		s := newTestSupervisor(WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s, quickCfg("r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)

		require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))

		ev := awaitSwap(t, swaps)
		require.NoError(t, ev.Error, "a standalone route change must apply")
		assert.NotSame(t, oldRt, s.Runtime(), "a real change must build and publish a new runtime")
	})
}
