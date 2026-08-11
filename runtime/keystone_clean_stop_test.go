package runtime_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// (runtime side): a deliberate Stop is a CLEAN pause, not an
// unrecoverable death. Before the KEYSTONE split, Stop set terminal=true on
// entry, so /live flipped to 503 and the liveness backstop killed the process
// even for a healthy, deliberate stop. After the split a clean Stop leaves the
// runtime non-terminal (only component-failure trips are terminal) while still
// being single-use (a stopped runtime cannot be restarted in place).
func TestStop_CleanDeliberateStop_IsNotTerminal(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("keystone-clean-stop"))

	ctx := context.Background()
	require.NoError(t, rt.Start(ctx))
	require.True(t, rt.IsRunning())
	require.False(t, rt.Terminal())

	require.NoError(t, rt.Stop(ctx))

	// Clean stop: not running, and crucially NOT terminal so /live stays 200
	// and the backstop does not restart the process.
	assert.False(t, rt.IsRunning(), "runtime must not be running after Stop")
	assert.False(t, rt.Terminal(),
		"a clean deliberate Stop must NOT report terminal")

	// The runtime is single-use: resume means the supervisor builds a NEW
	// runtime, so an in-place restart must be rejected (not silently reused).
	err := rt.Start(ctx)
	require.Error(t, err, "a stopped runtime is single-use and must reject restart")
	assert.False(t, rt.IsRunning(), "a rejected restart must not mark the runtime running")

	// Stop remains idempotent after a clean stop.
	assert.NoError(t, rt.Stop(ctx))
}
