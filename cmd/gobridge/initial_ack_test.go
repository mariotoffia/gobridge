package main

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
	"github.com/stretchr/testify/require"
)

func TestInitialAcknowledgementUsesFreshObservationIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &observationSource{changes: make(chan ports.ConfigObservation, 1)}
	manager := config.NewManager(config.Layer{Name: "target", Loader: source})
	observations, err := manager.Observe(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); manager.Stop() })
	observe := func(event ports.ConfigObservation) ports.ConfigObservation {
		source.changes <- event
		return wait.RequireReceive(t, observations, time.Second)
	}
	reusable := &ports.BridgeConfig{Version: 1, Bridge: ports.BridgeSettings{ID: "ack"}}
	first := observe(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: reusable})
	require.NotSame(t, reusable, first.Config)
	sup := bridge.NewSupervisor(bridge.WithSupervisorBlueprintValidator(config.Validate))
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx, first.Config, nil) }()
	t.Cleanup(func() { cancel(); require.NoError(t, wait.RequireReceive(t, done, time.Second)) })
	wait.Until(t, time.Second, "initial runtime published", func() bool { return sup.Runtime() != nil })
	session := &configSession{sup: sup, initial: first.Config}
	session.acknowledgeInitial(manager)
	version, ok := manager.RunningVersion()
	require.True(t, ok)
	require.Equal(t, 1, version)
	observe(ports.ConfigObservation{Kind: ports.ConfigMissing})
	manager.NotifyIdle()
	recreated := observe(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: reusable})
	require.NotSame(t, first.Config, recreated.Config)
	// A delayed acknowledgement from the previous lifetime cannot satisfy the
	// recreated document, even when the observer reused its original pointer.
	late := &configSession{sup: sup, initial: first.Config}
	late.acknowledgeInitial(manager)
	_, ok = manager.RunningVersion()
	require.False(t, ok)
}
