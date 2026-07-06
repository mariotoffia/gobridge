package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Finding L9: cancelling the context passed to Start (rather than calling Stop)
// left a dead runtime that still reported healthy — every background goroutine
// exited on the derived ctx but running/healthy stayed advertised, so /live and
// /ready lied. The fix watches the Start ctx and drives Stop on cancellation, so
// the runtime tears down and reports not-running.
func TestStart_ParentCtxCancel_DrivesStop(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("l9-ctx-cancel"))

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, rt.Start(ctx))
	require.True(t, rt.IsRunning())

	// Cancel the Start ctx WITHOUT calling Stop.
	cancel()

	// The watcher must drive Stop: the runtime stops running and goes terminal
	// instead of lingering as a dead-but-healthy runtime.
	require.Eventually(t, func() bool {
		return !rt.IsRunning() && rt.Terminal()
	}, 3*time.Second, 5*time.Millisecond,
		"cancelling the Start ctx must drive Stop, not leave a dead-but-healthy runtime")

	// A subsequent explicit Stop is idempotent (no panic, no error).
	assert.NoError(t, rt.Stop(context.Background()))
}
