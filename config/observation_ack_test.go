package config

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestManagerObservationReusedPointerCannotAcknowledgeNewEmit(t *testing.T) {
	for _, boundary := range []string{"consecutive present", "missing then present"} {
		t.Run(boundary, func(t *testing.T) {
			source := &observationLoader{observations: make(chan ports.ConfigObservation)}
			manager := NewManager(Layer{Name: "target", Loader: source})
			out, err := manager.Observe(t.Context())
			require.NoError(t, err)
			defer manager.Stop()
			send := func(o ports.ConfigObservation) ports.ConfigObservation {
				t.Helper()
				source.observations <- o
				return wait.RequireReceive(t, out, time.Second)
			}
			cfg := minimalValidConfig("reused-immutable-snapshot")
			cfg.Version = 7
			first := send(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: cfg})
			if boundary == "missing then present" {
				require.Equal(t, ports.ConfigMissing, send(ports.ConfigObservation{Kind: ports.ConfigMissing}).Kind)
				manager.NotifyIdle()
			}
			latest := send(ports.ConfigObservation{Kind: ports.ConfigPresent, Config: cfg})

			manager.NotifyApplyResult(first.Config, nil)
			_, running := manager.RunningVersion()
			require.False(t, running, "a late acknowledgement for the first emit must not confirm the latest emit")
			manager.NotifyApplyResult(first.Config, errors.New("late failure"))
			require.NoError(t, manager.LastApplyError())
			require.NotSame(t, first.Config, latest.Config)
			require.NotSame(t, cfg, latest.Config)
			require.Equal(t, cfg, latest.Config)
			manager.NotifyApplyResult(cfg, nil)
			_, running = manager.RunningVersion()
			require.False(t, running, "the source pointer is not an emitted acknowledgement identity")

			manager.NotifyApplyResult(latest.Config, nil)
			version, running := manager.RunningVersion()
			require.True(t, running)
			require.Equal(t, 7, version)
			require.False(t, manager.ReconfigurePending())
		})
	}
}
