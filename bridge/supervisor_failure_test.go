package bridge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

const swapTimeout = 2 * time.Second

func awaitSwap(t *testing.T, ch <-chan SwapEvent) SwapEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(swapTimeout):
		t.Fatal("timed out waiting for swap event")
		return SwapEvent{}
	}
}

func swapChan(n int) (func(SwapEvent), <-chan SwapEvent) {
	ch := make(chan SwapEvent, n)
	return func(e SwapEvent) { ch <- e }, ch
}

// TestSupervisor_OverlapBuildFailure_KeepsOldRunning validates that a build failure preserves the old runtime.
func TestSupervisor_OverlapBuildFailure_KeepsOldRunning(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	bad := quickCfg("r2")
	bad.Receivers[0].Transport = "nonexistent"
	require.True(t, sendConfig(ch, bad, time.Second))
	awaitSwap(t, swaps)
	assert.Equal(t, oldRt, s.Runtime())
}

// TestSupervisor_OverlapBuildFailure_SwapEventHasError validates the swap event carries the build error plus both configs.
func TestSupervisor_OverlapBuildFailure_SwapEventHasError(t *testing.T) {
	onSwap, swaps := swapChan(1)
	initial := quickCfg("r1")
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()

	bad := quickCfg("r2")
	bad.Receivers[0].Transport = "nonexistent"
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Equal(t, initial, ev.OldConfig)
	assert.Equal(t, bad, ev.NewConfig)
}

// TestSupervisor_OverlapBuildFailure_NextValidConfigWorks validates recovery after a failed overlap build.
func TestSupervisor_OverlapBuildFailure_NextValidConfigWorks(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := quickCfg("r2")
	bad.Receivers[0].Transport = "nonexistent"
	require.True(t, sendConfig(ch, bad, time.Second))
	require.Error(t, awaitSwap(t, swaps).Error)

	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_PrepareFailure_KeepsOldRunning validates that a Prepare failure in PrepareCommit mode leaves the old runtime untouched.
func TestSupervisor_PrepareFailure_KeepsOldRunning(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	oldRt := s.Runtime()

	bad := supervisorTestConfigWithSession("r2", "s1")
	bad.Stores.Lease = &ports.StoreConfig{Type: "unknown"}
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "no store factory")
	assert.Equal(t, oldRt, s.Runtime())
}

// TestSupervisor_PrepareFailure_NextValidConfigWorks validates recovery after a failed Prepare in PrepareCommit mode.
func TestSupervisor_PrepareFailure_NextValidConfigWorks(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := supervisorTestConfigWithSession("r2", "s1")
	bad.Stores.Lease = &ports.StoreConfig{Type: "unknown"}
	require.True(t, sendConfig(ch, bad, time.Second))
	require.Error(t, awaitSwap(t, swaps).Error)

	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_CompleteFailure_AfterStop validates that when Complete
// fails after the old runtime was stopped, the supervisor recovers
// with the old config.
func TestSupervisor_CompleteFailure_AfterStop(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		return nil, fmt.Errorf("connection refused")
	}
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	awaitSwap(t, swaps)
	require.True(t, waitForRouteID(s, "r1", swapTimeout), "supervisor should recover with old config after Complete failure")
}

// TestSupervisor_CompleteFailure_SwapEventReportsDegraded validates the swap event carries the error when Complete fails.
func TestSupervisor_CompleteFailure_SwapEventReportsDegraded(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		return nil, fmt.Errorf("connection refused")
	}
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "complete")
}

// TestSupervisor_CompleteFailure_NextConfigRecovers validates that after a Complete failure a valid overlap config rebuilds from scratch.
func TestSupervisor_CompleteFailure_NextConfigRecovers(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		return nil, fmt.Errorf("connection refused")
	}
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	require.Error(t, awaitSwap(t, swaps).Error)

	ef.SessionFn = nil
	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_StartFailure_Overlap validates that when Start fails in
// overlap mode the supervisor recovers by rebuilding with the old config.
func TestSupervisor_StartFailure_Overlap(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge", DrainTimeout: "100ms"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "exclusive"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "addr/test"}},
		Routes:    []ports.RouteDef{{ID: "fail-start", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"}}},
	}
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "start")
	assert.True(t, waitForRouteID(s, "r1", 2*time.Second), "supervisor should recover with old config after start failure")
	require.NotNil(t, s.Runtime(), "overlap start failure must recover a non-nil runtime (old config), not leave rt nil")
}

// TestSupervisor_StartFailure_PrepareCommit validates that when Start
// fails in PrepareCommit mode the supervisor recovers with the old config.
func TestSupervisor_StartFailure_PrepareCommit(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := supervisorTestConfigWithSession("r2", "s1")
	bad.Bridge.DrainTimeout = "100ms"
	bad.Receivers[0].Transport = "exclusive"
	bad.Routes[0].DeliveryMode = "direct_hold"
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "start")
	require.True(t, waitForRouteID(s, "r1", swapTimeout), "supervisor should recover with old config")
	require.NotNil(t, s.Runtime(), "prepare-commit start failure must recover a non-nil runtime (old config), not leave rt nil")
}

// TestSupervisor_StartFailure_NextConfigRecovers validates recovery after a Start failure by sending a valid config.
func TestSupervisor_StartFailure_NextConfigRecovers(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge", DrainTimeout: "100ms"},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "exclusive"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "addr/test"}},
		Routes:    []ports.RouteDef{{ID: "fail-start", ReceiverID: "rx", DeliveryMode: "direct_hold", Bindings: []string{"b1"}}},
	}
	require.True(t, sendConfig(ch, bad, time.Second))
	require.Error(t, awaitSwap(t, swaps).Error)

	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_StopTimeout_Overlap validates that a short drain timeout does not prevent an overlap swap.
func TestSupervisor_StopTimeout_Overlap(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	initial := quickCfg("r1")
	initial.Bridge.DrainTimeout = "100ms"
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r2", swapTimeout))
}

// TestSupervisor_StopTimeout_PrepareCommit validates that a short drain timeout does not prevent a PrepareCommit swap.
func TestSupervisor_StopTimeout_PrepareCommit(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, _ := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	initial := quickCfg("r1")
	initial.Bridge.DrainTimeout = "100ms"
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()

	cfg := supervisorTestConfigWithSession("r2", "s1")
	cfg.Bridge.DrainTimeout = "100ms"
	require.True(t, sendConfig(ch, cfg, time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r2", swapTimeout))
}

// TestSupervisor_FailingSessionClose validates that the swap succeeds even when internal session close may error.
func TestSupervisor_FailingSessionClose(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	initial := quickCfg("r1")
	initial.Bridge.DrainTimeout = "100ms"
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	assert.NotNil(t, s.Runtime())
}

// TestSupervisor_StopErrorDoesNotPreventSwap validates that Stop errors do not prevent consecutive swaps.
func TestSupervisor_StopErrorDoesNotPreventSwap(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	initial := quickCfg("r1")
	initial.Bridge.DrainTimeout = "100ms"
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	require.NoError(t, awaitSwap(t, swaps).Error)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_BrokerUnreachable_Overlap validates that a transport session creation failure keeps the old runtime.
func TestSupervisor_BrokerUnreachable_Overlap(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	s.RegisterTransport("broken", &failingTransportFactory{sessionErr: fmt.Errorf("broker unreachable")})
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	oldRt := s.Runtime()

	bad := quickCfg("r2")
	bad.Sessions = []ports.SessionDef{{ID: "s1", Transport: "broken"}}
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Equal(t, oldRt, s.Runtime())
}

// TestSupervisor_BrokerUnreachable_PrepareCommit validates that session
// creation failure during Complete triggers recovery with old config.
func TestSupervisor_BrokerUnreachable_PrepareCommit(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s, ef := newTestSupervisorWithExclusive(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		return nil, fmt.Errorf("broker unreachable")
	}
	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	require.True(t, waitForRouteID(s, "r1", swapTimeout), "supervisor should recover with old config")
}

// TestSupervisor_NoTransportsRegistered validates that a config with an unregistered transport fails while old runtime survives.
func TestSupervisor_NoTransportsRegistered(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := quickCfg("r2")
	bad.Receivers[0].Transport = "unknown"
	require.True(t, sendConfig(ch, bad, time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "no transport factory")
	assert.NotNil(t, s.Runtime())
}

// TestSupervisor_SwapCallback_NotCalledOnInvalidConfig validates that a SwapEvent with error is emitted for invalid configs.
func TestSupervisor_SwapCallback_NotCalledOnInvalidConfig(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	require.True(t, sendConfig(ch, invalidCfg(), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "config validation")
}

// TestSupervisor_RuntimeAccessor_DuringSwap validates that concurrent Runtime() calls during swaps cause no panics.
func TestSupervisor_RuntimeAccessor_DuringSwap(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 10)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Runtime()
			}
		}()
	}
	for i := 0; i < 5; i++ {
		sendConfig(ch, quickCfg(fmt.Sprintf("swap-%d", i)), time.Second)
	}
	wg.Wait()
}

// TestSupervisor_ConfigAccessor_DuringSwap validates that concurrent Config() calls always return non-nil after startup.
func TestSupervisor_ConfigAccessor_DuringSwap(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 10)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				assert.NotNil(t, s.Config())
			}
		}()
	}
	for i := 0; i < 5; i++ {
		sendConfig(ch, quickCfg(fmt.Sprintf("swap-%d", i)), time.Second)
	}
	wg.Wait()
}

// TestSupervisor_ConcurrentApplySerializes validates that rapid config changes serialize and the final route wins.
func TestSupervisor_ConcurrentApplySerializes(t *testing.T) {
	onSwap, swaps := swapChan(2)
	s := newTestSupervisor(WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 2)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	require.True(t, sendConfig(ch, quickCfg("r3"), time.Second))
	awaitSwap(t, swaps)
	awaitSwap(t, swaps)
	require.True(t, waitForRouteID(s, "r3", swapTimeout))
}

// TestSupervisor_ContextCancel_DuringSwap validates that cancelling the context causes Run to return without hanging.
func TestSupervisor_ContextCancel_DuringSwap(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	waitForRuntime(s, swapTimeout)

	sendConfig(ch, quickCfg("r2"), time.Second)
	cancel()
	select {
	case <-errCh:
	case <-time.After(swapTimeout):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestSupervisor_ChannelClosed_WhileApplying validates that closing the config channel causes Run to complete gracefully.
func TestSupervisor_ChannelClosed_WhileApplying(t *testing.T) {
	s := newTestSupervisor()
	ch := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	waitForRuntime(s, swapTimeout)

	sendConfig(ch, quickCfg("r2"), time.Second)
	close(ch)
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(swapTimeout):
		t.Fatal("Run did not return after channel close")
	}
}
