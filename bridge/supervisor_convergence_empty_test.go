package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestRuntimeConverged_EmptyRunningRuntimeHasNothingToConverge proves a
// deliberately empty configuration converges. The convergence watch exists to
// catch sessions that never reach their broker; an instance with no routes and
// no sessions has none, and it is capped below LevelSubscribed precisely
// because it bridges nothing. Demanding LevelSubscribed from it would leave
// every empty-config deployment permanently marked applied-but-not-converged,
// and would stall a coordinated rollout's confirm window forever.
func TestRuntimeConverged_EmptyRunningRuntimeHasNothingToConverge(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("converged-empty"))
	require.NoError(t, rt.Start(t.Context()))
	t.Cleanup(func() { _ = rt.Stop(t.Context()) })

	converged, level := runtimeConverged(t.Context(), rt)

	assert.True(t, converged, "a running runtime that carries nothing has nothing left to converge")
	assert.Equal(t, ports.LevelRunning, level,
		"the reported level stays the honest one, so a degraded reason would name it accurately")
}

// TestRuntimeConverged_EmptyUnstartedRuntimeIsNotConverged proves "empty" is
// not a blanket pass: a runtime that has not started has not converged, which
// is the exact shape of a committed reload whose transport never came up.
func TestRuntimeConverged_EmptyUnstartedRuntimeIsNotConverged(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("unconverged-empty"))

	converged, level := runtimeConverged(t.Context(), rt)

	assert.False(t, converged, "an unstarted runtime has not converged, empty or not")
	assert.Equal(t, ports.LevelLive, level)
}
