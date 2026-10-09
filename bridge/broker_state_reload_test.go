package bridge

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// startBrokerStateSupervisor runs a Supervisor over factory as the "identity"
// transport, started on cfg. stop cancels it and waits for Run to return; it
// also runs when the test ends.
func startBrokerStateSupervisor(t *testing.T, factory *brokerStateFactory, cfg *ports.BridgeConfig, opts ...SupervisorOption,
) (s *Supervisor, changes chan *ports.BridgeConfig, swaps <-chan SwapEvent, stop func()) {
	t.Helper()
	onSwap, swaps := swapChan(4)
	s = NewSupervisor(append([]SupervisorOption{WithOnSwap(onSwap)}, opts...)...)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("identity", factory)
	changes = make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := runSupervisorAsync(ctx, s, cfg, changes)
	stop = sync.OnceFunc(func() { cancel(); <-errCh })
	t.Cleanup(stop)
	wait.Until(t, 5*time.Second, "the initial runtime starts", func() bool { return s.Runtime() != nil })
	return s, changes, swaps, stop
}

// recordAddedKeys makes factory's sessions record the broker state key of each
// session built with SessionSpec.BrokerStateKeyAdded, and returns a reader.
func recordAddedKeys(factory *brokerStateFactory) func() []string {
	var (
		mu    sync.Mutex
		added []string
	)
	factory.SessionFn = func(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
		key := brokerStateTestKey(spec)
		if spec.BrokerStateKeyAdded {
			mu.Lock()
			added = append(added, key)
			mu.Unlock()
		}
		return &brokerStateSession{key: key, log: factory.log}, nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(added)
	}
}

func TestSupervisor_InPlaceReloadEndsTheBrokerStateOfAChangedIdentity(t *testing.T) {
	factory := newBrokerStateFactory()
	added := recordAddedKeys(factory)
	_, changes, swaps, _ := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"))

	ev := reloadTo(t, changes, swaps, configWithDurableSessionIdentity(2, "opaque-b"), 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Equal(t, []string{"end:identity:opaque-a", "close:identity:opaque-a"}, factory.log.of("identity:opaque-a"))
	assert.Equal(t, []string{"identity:opaque-b"}, added(), "the new identity starts clean")
}

func TestSupervisor_InPlaceReloadEndsTheBrokerStateOfARemovedSession(t *testing.T) {
	factory := newBrokerStateFactory()
	_, changes, swaps, _ := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"))

	ev := reloadTo(t, changes, swaps, supervisorTestConfig("r1"), 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, []string{"end:identity:opaque-a", "close:identity:opaque-a"}, factory.log.of("identity:opaque-a"))
}

func TestSupervisor_InPlaceReloadKeepsTheBrokerStateOfARenamedSession(t *testing.T) {
	factory := newBrokerStateFactory()
	added := recordAddedKeys(factory)
	_, changes, swaps, _ := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"))

	next := renamedDurableSession(configWithDurableSessionIdentity(2, "opaque-a"), "renamed-session")
	ev := reloadTo(t, changes, swaps, next, 2)

	require.NoError(t, ev.Error)
	assert.False(t, factory.log.ended(), "the broker identity is still in the next configuration")
	assert.Empty(t, added())
}

func TestSupervisor_FullSwapEndsTheBrokerStateOfAChangedIdentity(t *testing.T) {
	for name, mode := range map[string]SwapMode{"overlap": SwapOverlap, "prepare-commit": SwapPrepareCommit} {
		t.Run(name, func(t *testing.T) {
			factory := newBrokerStateFactory()
			added := recordAddedKeys(factory)
			_, changes, swaps, _ := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"), WithSwapMode(mode))

			ev := reloadTo(t, changes, swaps, configWithDurableSessionIdentity(2, "opaque-b"), 2)

			require.NoError(t, ev.Error)
			assert.Equal(t, mode, ev.SwapMode)
			assert.Equal(t, []string{"end:identity:opaque-a", "close:identity:opaque-a"}, factory.log.of("identity:opaque-a"))
			assert.Equal(t, []string{"identity:opaque-b"}, added())
		})
	}
}

func TestSupervisor_ShutdownEndsNoBrokerState(t *testing.T) {
	factory := newBrokerStateFactory()
	_, _, _, stop := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"))

	stop()

	assert.Equal(t, []string{"close:identity:opaque-a"}, factory.log.of("identity:opaque-a"))
	assert.False(t, factory.log.ended())
}

func TestSupervisor_PauseEndsNoBrokerState(t *testing.T) {
	factory := newBrokerStateFactory()
	s, _, _, _ := startBrokerStateSupervisor(t, factory, configWithDurableSessionIdentity(1, "opaque-a"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.StopBridge(ctx))

	assert.Equal(t, []string{"close:identity:opaque-a"}, factory.log.of("identity:opaque-a"))
	assert.False(t, factory.log.ended())
}
