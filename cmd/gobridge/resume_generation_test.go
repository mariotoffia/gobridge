package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/require"
)

func TestResumeRejectsStaleObservationGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &observationSource{changes: make(chan ports.ConfigObservation, 1)}
	manager := config.NewManager(config.Layer{Name: "target", Loader: source})
	observations, err := manager.Observe(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); manager.Stop() })
	observe := func(version int) *ports.BridgeConfig {
		source.changes <- ports.ConfigObservation{Kind: ports.ConfigPresent, Config: &ports.BridgeConfig{Version: version, Bridge: ports.BridgeSettings{ID: "resume-generation"}}}
		return wait.RequireReceive(t, observations, time.Second).Config
	}
	initial := observe(1)
	pipeline := newReloadPipeline(ports.NewRegistry(), nil, withApplyResultNotifier(manager))
	swapped := make(chan bridge.SwapEvent, 1)
	sup := bridge.NewSupervisor(bridge.WithReconfigStrategy(bridge.NewDirectStrategy()), bridge.WithOnSwap(func(ev bridge.SwapEvent) { pipeline.onSwap(ev); swapped <- ev }))
	changes, done := make(chan *ports.BridgeConfig, 1), make(chan error, 1)
	go func() { done <- sup.Run(ctx, initial, changes) }()
	t.Cleanup(func() { cancel(); require.NoError(t, wait.RequireReceive(t, done, time.Second)) })
	wait.Until(t, time.Second, "initial runtime", func() bool { return sup.Runtime() != nil })
	session := &configSession{sup: sup, initial: initial}
	session.recordObservation(initial, 0)
	session.acknowledgeInitial(manager)
	var current atomic.Pointer[configSession]
	var absent atomic.Bool
	var generation atomic.Uint64
	current.Store(session)
	controller := observedController{current: &current, absent: &absent, generation: &generation, manager: manager}
	require.NoError(t, controller.StopBridge(ctx))
	next := observe(2)
	session.recordObservation(next, 0)
	changes <- next
	require.True(t, wait.RequireReceive(t, swapped, time.Second).Deferred)
	require.True(t, manager.ReconfigurePending())
	generation.Add(1)
	require.Error(t, controller.StartBridge(ctx), "a stale generation cannot resume or acknowledge its configuration")
	require.False(t, sup.Runtime().IsRunning())
	version, ok := manager.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 1, version)
	require.True(t, manager.ReconfigurePending())
}
