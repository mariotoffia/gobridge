package runtime_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestDeepHealth_RuntimeWithoutRoutesReportsEmpty proves a started runtime that
// carries no routes and no sessions marks itself empty and refuses to advertise
// traffic readiness. This is the start-empty state: the process is alive and
// healthy, but a missing or route-less config means not a single message can be
// bridged, so nothing may steer traffic at it or gate a rollout on it.
func TestDeepHealth_RuntimeWithoutRoutesReportsEmpty(t *testing.T) {
	rt := newTestRuntime("bridge-empty", nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	dh := rt.DeepHealth(ctx)

	require.True(t, dh.Running, "precondition: the runtime is started")
	require.True(t, dh.Healthy, "precondition: an empty runtime has no component errors")
	assert.True(t, dh.Empty,
		"a runtime with zero routes and zero sessions must report itself empty")
	assert.False(t, dh.ReadyForTraffic,
		"an instance that bridges nothing must not advertise itself as ready for traffic")
	assert.Equal(t, ports.LevelRunning, ports.ReadinessLevelFromDeepHealth(dh),
		"an empty instance must not reach LevelFull")
}
