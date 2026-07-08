package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// counterHasState reports whether entries contains a counter tagged with
// TagKeyState == state.
func counterHasState(entries []ports.MetricEntry, state string) bool {
	for _, e := range entries {
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeyState && tag.Value == state {
				return true
			}
		}
	}
	return false
}

// gaugeHasValue reports whether entries contains a gauge recorded with value v.
func gaugeHasValue(entries []ports.MetricEntry, v float64) bool {
	for _, e := range entries {
		if e.FValue == v {
			return true
		}
	}
	return false
}

// TestSupervisor_StreamClose_ExportsDegradedGauge asserts that losing the
// config-change stream — previously visible only in the logs — now also flips
// the exported ConfigDegraded gauge to 1 so operators can alert on a bridge
// running blind on its last good config (Finding 4).
func TestSupervisor_StreamClose_ExportsDegradedGauge(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newTestSupervisor(WithSupervisorMetrics(rec))
	ch := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	require.NotNil(t, waitForRuntime(s, swapTimeout))

	// Config-change stream lost while ctx is still live: supervisor keeps
	// serving but goes degraded.
	close(ch)

	// The gauge is emitted immediately after markDegraded flips the state, so
	// its appearance implies Degraded() is already true.
	require.Eventually(t, func() bool {
		return gaugeHasValue(rec.FindEntries(shared.MetricConfigDegraded), 1)
	}, swapTimeout, 10*time.Millisecond, "degraded gauge must be exported as 1 on stream close")

	degraded, _ := s.Degraded()
	assert.True(t, degraded)

	cancel()
	select {
	case <-errCh:
	case <-time.After(swapTimeout):
		t.Fatal("Run did not return after cancel")
	}
}

// TestSupervisor_SuccessfulReload_ExportsReloadSuccess asserts a live
// reconfiguration exports a success-tagged reload counter and resets the
// degraded gauge to 0 (Finding 4).
func TestSupervisor_SuccessfulReload_ExportsReloadSuccess(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))
	// The swap event fires AFTER apply emits its metrics, so awaitSwap makes the
	// assertions deterministic without any sleep.
	require.NoError(t, awaitSwap(t, swaps).Error)

	assert.True(t, counterHasState(rec.FindEntries(shared.MetricConfigReloads), "success"),
		"successful reload must export a success-tagged counter")
	assert.True(t, gaugeHasValue(rec.FindEntries(shared.MetricConfigDegraded), 0),
		"successful reload must reset the degraded gauge to 0")
}

// TestSupervisor_FailedReload_ExportsReloadFailure asserts a rejected live
// reconfiguration exports a failure-tagged reload counter so a config that
// keeps being rejected by the running runtime is observable (Finding 4).
func TestSupervisor_FailedReload_ExportsReloadFailure(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithSupervisorMetrics(rec), WithOnSwap(onSwap))
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, quickCfg("r1"), ch)
	defer cancel()

	bad := quickCfg("r2")
	bad.Receivers[0].Transport = "nonexistent"
	require.True(t, sendConfig(ch, bad, time.Second))
	require.Error(t, awaitSwap(t, swaps).Error)

	assert.True(t, counterHasState(rec.FindEntries(shared.MetricConfigReloads), "failure"),
		"failed reload must export a failure-tagged counter")
}

// TestSupervisor_StreamClose_NilMetricsNoPanic asserts the degraded emission is
// a safe no-op when no metrics exporter is wired: the goroutine must not panic
// (the nil-guard in emitConfigDegradedGauge) and Degraded() still flips.
func TestSupervisor_StreamClose_NilMetricsNoPanic(t *testing.T) {
	s := newTestSupervisor() // no WithSupervisorMetrics -> s.metrics is nil
	ch := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := runSupervisorAsync(ctx, s, quickCfg("r1"), ch)
	require.NotNil(t, waitForRuntime(s, swapTimeout))

	close(ch)
	require.Eventually(t, func() bool {
		degraded, _ := s.Degraded()
		return degraded
	}, swapTimeout, 10*time.Millisecond, "degraded must be set even without a metrics exporter")

	cancel()
	select {
	case <-errCh:
	case <-time.After(swapTimeout):
		t.Fatal("Run did not return after cancel")
	}
}
