package runtime

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetire_ForgetsASharedSessionOnlyWithItsLastRoute pins that credentials
// keep rotating into a session object a surviving hand-wired route still rides
// on: retiring one of two routes added with it does not forget it, and
// retiring the other does.
func TestRetire_ForgetsASharedSessionOnlyWithItsLastRoute(t *testing.T) {
	rt := New(WithInstanceID("retire-shared-credentials"))
	common, recv1 := newRetireSession(), newComponentReceiver()
	require.NoError(t, rt.AddRoute(componentRoute("r1"), recv1, &componentSender{}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, common, nil))
	var forgotten []any
	rt.AttachCredentialForget(func(targets []any) bool { forgotten = targets; return false })
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Routes: []string{"r1"}})

	assert.True(t, slices.Contains(forgotten, any(recv1)), "the retired route's receiver is forgotten")
	assert.False(t, slices.Contains(forgotten, any(common)), "a session a surviving route rides on is still watched")

	retire(t, rt, Unit{Routes: []string{"r2"}})

	assert.True(t, slices.Contains(forgotten, any(common)), "retiring the session's last route forgets it")
}
