package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type stubLoader struct {
	cfg *ports.BridgeConfig
	err error
}

func (s *stubLoader) Load(_ context.Context) (*ports.BridgeConfig, error) {
	return s.cfg, s.err
}

type stubWatcher struct {
	ch  chan *ports.BridgeConfig
	err error
}

func (s *stubWatcher) Watch(_ context.Context) (<-chan *ports.BridgeConfig, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.ch, nil
}

func minimalValidConfig(id string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: id},
		Receivers: []ports.ReceiverDef{
			{ID: "recv1", Transport: "mqtt"},
		},
		Senders: []ports.SenderDef{
			{ID: "snd1", Transport: "mqtt"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind1", SenderID: "snd1", Address: "topic/out"},
		},
		Routes: []ports.RouteDef{
			{ID: "route1", ReceiverID: "recv1", Bindings: []string{"bind1"}},
		},
	}
}

func TestManager_Load_BaseOnly(t *testing.T) {
	base := minimalValidConfig("base-bridge")

	mgr := NewManager(Layer{
		Name:   "file",
		Loader: &stubLoader{cfg: base},
	})

	cfg, err := mgr.Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "base-bridge", cfg.Bridge.ID)
}

func TestManager_Load_WithOverlay(t *testing.T) {
	base := minimalValidConfig("base-bridge")
	base.Bridge.LogLevel = "info"

	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{LogLevel: "debug"},
	}

	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: base}},
		WithOverlay(Layer{Name: "remote", Loader: &stubLoader{cfg: overlay}}),
	)

	cfg, err := mgr.Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "base-bridge", cfg.Bridge.ID)
	assert.Equal(t, "debug", cfg.Bridge.LogLevel)
}

func TestManager_Load_OverlayReplacesStores(t *testing.T) {
	base := minimalValidConfig("bridge1")
	base.Bridge.DeploymentMode = "clustered"
	base.Stores = ports.StoresConfig{
		Lease:  &ports.StoreConfig{Type: "memory"},
		Outbox: &ports.StoreConfig{Type: "memory"},
	}

	overlay := &ports.BridgeConfig{
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "dynamodb"},
			Outbox: &ports.StoreConfig{Type: "dynamodb"},
		},
	}

	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: base}},
		WithOverlay(Layer{Name: "ddb", Loader: &stubLoader{cfg: overlay}}),
	)

	cfg, err := mgr.Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dynamodb", cfg.Stores.Lease.Type)
	assert.Equal(t, "dynamodb", cfg.Stores.Outbox.Type)
}

func TestManager_Load_ValidationFailure(t *testing.T) {
	base := &ports.BridgeConfig{}

	mgr := NewManager(Layer{
		Name:   "file",
		Loader: &stubLoader{cfg: base},
	})

	_, err := mgr.Load(context.Background())
	require.Error(t, err, "missing bridge.id should fail validation")
}

func TestManager_Watch_EmitsOnChange(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := mgr.Watch(ctx)
	require.NoError(t, err)

	updated := minimalValidConfig("bridge1-updated")
	watchCh <- updated

	select {
	case cfg := <-out:
		require.NotNil(t, cfg)
		assert.Equal(t, "bridge1-updated", cfg.Bridge.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}

	mgr.Stop()
}

func TestManager_Watch_InvalidMergedConfigDropped(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := mgr.Watch(ctx)
	require.NoError(t, err)

	// Push an invalid config (missing bridge.id)
	watchCh <- &ports.BridgeConfig{}

	select {
	case <-out:
		t.Fatal("invalid config should not be emitted")
	case <-time.After(200 * time.Millisecond):
		// expected: invalid merged config was dropped
	}

	mgr.Stop()
}

func TestManager_Watch_AlreadyRunning(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = mgr.Watch(ctx)
	require.NoError(t, err)

	_, err = mgr.Watch(ctx)
	require.Error(t, err, "second Watch should fail")

	mgr.Stop()
}

func TestManager_StopWaitsForLoopExit(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx := context.Background()

	_, err = mgr.Watch(ctx)
	require.NoError(t, err)

	mgr.Stop()

	// After Stop returns, Watch must succeed immediately —
	// the old loop is fully exited, running is false.
	out, err := mgr.Watch(ctx)
	require.NoError(t, err, "Watch after Stop should succeed because old loop has exited")
	require.NotNil(t, out)

	mgr.Stop()
}

func TestManager_StopThenWatch_NoRace(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 1)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	// Rapid stop/restart cycles must not race.
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		out, err := mgr.Watch(ctx)
		require.NoError(t, err)

		mgr.Stop()
		// Output channel must be closed after Stop returns.
		_, open := <-out
		assert.False(t, open, "output channel should be closed after Stop")
		cancel()
	}
}

func TestManager_Watch_SlowConsumer_GetsLatestConfig(t *testing.T) {
	base := minimalValidConfig("bridge1")
	watchCh := make(chan *ports.BridgeConfig, 4)

	mgr := NewManager(Layer{
		Name:    "file",
		Loader:  &stubLoader{cfg: base},
		Watcher: &stubWatcher{ch: watchCh},
	})

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := mgr.Watch(ctx)
	require.NoError(t, err)

	// Push three configs rapidly without consuming. A single supervisor
	// goroutine forwards watchCh in order and the watch loop processes events
	// serially, so each emit drains the stale buffered config: the out buffer
	// deterministically ends holding v3.
	cfg1 := minimalValidConfig("v1")
	cfg2 := minimalValidConfig("v2")
	cfg3 := minimalValidConfig("v3")

	watchCh <- cfg1
	watchCh <- cfg2
	watchCh <- cfg3
	// SYNC sentinel: an invalid config (missing bridge.id) is processed
	// strictly after cfg3's emit completed (serial loop); it is dropped
	// without touching out and records a layer watch error. WatchDegraded()
	// therefore proves v3 is the buffered config before we read.
	watchCh <- &ports.BridgeConfig{}
	wait.Until(t, 2*time.Second, "watch loop processed all pushed configs", func() bool {
		return mgr.WatchDegraded()
	})

	// Now read: should get the latest config (v3), not v1.
	select {
	case cfg := <-out:
		require.NotNil(t, cfg)
		assert.Equal(t, "v3", cfg.Bridge.ID,
			"slow consumer should receive latest config, not stale one")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config")
	}

	mgr.Stop()
}

func TestManager_CustomMergeFunc(t *testing.T) {
	base := minimalValidConfig("bridge1")
	overlay := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{LogLevel: "custom"},
	}

	called := false
	customMerge := func(b, o *ports.BridgeConfig) (*ports.BridgeConfig, error) {
		called = true
		return DefaultMerge(b, o)
	}

	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: base}},
		WithOverlay(Layer{Name: "remote", Loader: &stubLoader{cfg: overlay}}),
		WithMergeFunc(customMerge),
	)

	cfg, err := mgr.Load(context.Background())
	require.NoError(t, err)
	assert.True(t, called, "custom merge should be called")
	assert.Equal(t, "custom", cfg.Bridge.LogLevel)
}
