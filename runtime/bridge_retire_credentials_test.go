package runtime

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
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

// forgetRecorder is a credential refresher's forget that records every target
// it is told to forget, from any Retire, and never goes idle.
type forgetRecorder struct {
	mu      sync.Mutex
	targets []any
}

func (r *forgetRecorder) forget(targets []any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, targets...)
	return false
}

// count returns how often target was forgotten.
func (r *forgetRecorder) count(target any) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(slices.DeleteFunc(slices.Clone(r.targets), func(t any) bool { return t != target }))
}

// TestRetire_KeepsWatchingASessionARetiringRouteUses pins that credentials keep
// rotating into a session object a route still draining in another Retire rides
// on: retiring the second of two routes added with it, while the first's Retire
// drains, does not forget it, and the first Retire forgets it once.
func TestRetire_KeepsWatchingASessionARetiringRouteUses(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	rt := New(WithInstanceID("retire-overlap-credentials"))
	common, recv1 := newRetireSession(), newComponentReceiver()
	require.NoError(t, rt.AddRoute(componentRoute("r1"), recv1, &componentSender{release: release}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, common, nil))
	recorder := &forgetRecorder{}
	rt.AttachCredentialForget(recorder.forget)
	startComponentRuntime(t, rt)
	first := retireHeldInDrain(t, rt, recv1, Unit{Routes: []string{"r1"}})

	retire(t, rt, Unit{Routes: []string{"r2"}})

	assert.Zero(t, recorder.count(common), "a session a still-draining route rides on is still watched")
	releaseOnce()
	require.NoError(t, wait.RequireReceive(t, first, 5*time.Second))
	assert.Equal(t, 1, recorder.count(common), "the Retire that lets go last forgets the session once")
}

// TestRetire_ARetireThatLetGoOfASharedSessionLeavesItToTheLast pins that a unit
// stops keeping its credential targets watched once its Retire has let go of
// them, even while its components are still stopping: a Retire that lets go
// first leaves the shared session to a Retire still draining on it, which then
// forgets it once instead of both keeping it watched.
func TestRetire_ARetireThatLetGoOfASharedSessionLeavesItToTheLast(t *testing.T) {
	drain, stuck := make(chan struct{}), make(chan struct{})
	releaseDrain := sync.OnceFunc(func() { close(drain) })
	releaseStuck := sync.OnceFunc(func() { close(stuck) })
	t.Cleanup(releaseDrain)
	t.Cleanup(releaseStuck)
	rt := New(WithInstanceID("retire-overlap-let-go"))
	common, recv1, recv2 := newRetireSession(), newComponentReceiver(), stuckReceiver{release: stuck}
	require.NoError(t, rt.AddRoute(componentRoute("r1"), recv1, &componentSender{release: drain}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), recv2, &componentSender{}, common, nil))
	recorder := &forgetRecorder{}
	rt.AttachCredentialForget(recorder.forget)
	startComponentRuntime(t, rt)
	draining := retireHeldInDrain(t, rt, recv1, Unit{Routes: []string{"r1"}})
	stopping := retireAsync(t, rt, Unit{Routes: []string{"r2"}})
	wait.Until(t, 2*time.Second, "the second Retire lets go of its route's receiver", func() bool {
		return recorder.count(recv2) == 1
	})
	assert.Zero(t, recorder.count(common), "a session a still-draining route rides on is still watched")

	releaseDrain()
	require.NoError(t, wait.RequireReceive(t, draining, 5*time.Second))
	releaseStuck()
	require.NoError(t, wait.RequireReceive(t, stopping, 5*time.Second))

	assert.Equal(t, 1, recorder.count(common), "the Retire that lets go last forgets the session once")
}
