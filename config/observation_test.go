package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestManagerObservesMissingWithoutLosingRunningState(t *testing.T) {
	source := &observationLoader{observations: make(chan ports.ConfigObservation)}
	m := NewManager(Layer{Name: "target", Loader: source})
	out, err := m.Observe(t.Context())
	require.NoError(t, err)
	defer m.Stop()
	send := func(o ports.ConfigObservation) ports.ConfigObservation {
		t.Helper()
		source.observations <- o
		return wait.RequireReceive(t, out, time.Second)
	}
	a := minimalValidConfig("first")
	a.Version = 9
	got := send(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: a})
	require.Equal(t, a, got.Config)
	require.NotSame(t, a, got.Config)
	m.NotifyApplyResult(got.Config, nil)
	require.Equal(t, ports.ConfigReadError, send(ports.ConfigObservation{Kind: ports.ConfigReadError, Err: shared.ErrUnavailable}).Kind)
	require.True(t, m.WatchDegraded())
	v, ok := m.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 9, v)
	require.Equal(t, ports.ConfigMissing, send(ports.ConfigObservation{Kind: ports.ConfigMissing, Err: shared.ErrNotFound}).Kind)
	_, ok = m.AppliedVersion()
	require.False(t, ok)
	m.NotifyIdle()
	m.NotifyApplyResult(got.Config, nil) // late acknowledgement must not resurrect it.
	_, ok = m.RunningVersion()
	require.False(t, ok)
	b := minimalValidConfig("recreated")
	b.Version = 1
	got = send(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: b})
	require.Equal(t, b, got.Config)
	m.NotifyApplyResult(got.Config, nil)
	v, ok = m.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 1, v)
	require.False(t, m.WatchDegraded())
	require.False(t, m.ReconfigurePending())
}

func TestManagerObservationRejectsAmbiguousLayers(t *testing.T) {
	m := NewManager(Layer{}, WithOverlay(Layer{}))
	_, err := m.Observe(t.Context())
	require.ErrorIs(t, err, shared.ErrNotSupported)
}

func TestManagerObservationRestartsWithoutResettingSequence(t *testing.T) {
	fc := clocktest.New()
	first, second := make(chan ports.ConfigObservation, 1), make(chan ports.ConfigObservation, 1)
	source := &restartingObserver{channels: make(chan chan ports.ConfigObservation, 2)}
	source.channels <- first
	source.channels <- second
	first <- ports.ConfigObservation{Kind: ports.ConfigMissing}
	close(first)
	second <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: minimalValidConfig("recovered"), Sequence: 1}
	m := NewManager(Layer{Name: "target", Loader: source}, WithManagerClock(fc))
	out, err := m.Observe(t.Context())
	require.NoError(t, err)
	defer m.Stop()
	firstObservation := wait.RequireReceive(t, out, time.Second)
	fault := wait.RequireReceive(t, out, time.Second)
	require.Equal(t, ports.ConfigMissing, firstObservation.Kind)
	require.Equal(t, ports.ConfigReadError, fault.Kind)
	require.True(t, m.WatchDegraded())
	wait.Until(t, time.Second, "observation retry armed", func() bool { return fc.TimerCount() == 1 })
	fc.Advance(watchRetryInitial)
	recovered := wait.RequireReceive(t, out, time.Second)
	require.Equal(t, ports.ConfigPresent, recovered.Kind)
	require.Greater(t, recovered.Sequence, fault.Sequence)
	require.False(t, m.WatchDegraded())
}

func TestManagerInvalidObservationRetainsDesiredAndStopUnblocksDelivery(t *testing.T) {
	source := &observationLoader{observations: make(chan ports.ConfigObservation, 3)}
	valid := minimalValidConfig("retained")
	source.observations <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: valid}
	source.observations <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: &ports.BridgeConfig{}}
	source.observations <- ports.ConfigObservation{Kind: ports.ConfigMissing}
	m := NewManager(Layer{Name: "target", Loader: source})
	out, err := m.Observe(t.Context())
	require.NoError(t, err)
	require.Equal(t, valid, wait.RequireReceive(t, out, time.Second).Config)
	fault := wait.RequireReceive(t, out, time.Second)
	require.Equal(t, ports.ConfigReadError, fault.Kind)
	m.Stop() // Missing may be blocked behind the unbuffered consumer.
	wait.RequireClosed(t, out, time.Second)
}
