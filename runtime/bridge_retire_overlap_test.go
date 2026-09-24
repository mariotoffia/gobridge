package runtime

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// retireHeldInDrain starts retiring u while a delivery emitted on recv, the
// receiver of one of u's routes, is in flight, and returns once u's routes have
// left rt. That route's sender must hold its sends, so the Retire waits in its
// drain until the test releases them; the channel yields the Retire's result.
func retireHeldInDrain(t *testing.T, rt *Runtime, recv *componentReceiver, u Unit) <-chan error {
	t.Helper()
	wait.RequireClosed(t, recv.ready, 2*time.Second)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "in-flight", Subject: "retire"})
	// Emitted under the receiver's run context, as a transport does.
	require.NoError(t, recv.emit(recv.ctx, &ackDelivery{syntheticDelivery: syntheticDelivery{env: env}}))
	return retireAsync(t, rt, u)
}

// retireAsync starts retiring u and returns once u's routes have left rt; the
// channel yields the Retire's result.
func retireAsync(t *testing.T, rt *Runtime, u Unit) <-chan error {
	t.Helper()
	retired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		retired <- rt.Retire(ctx, u)
	}()
	wait.Until(t, 2*time.Second, "Retire takes the unit out", func() bool {
		return !slices.ContainsFunc(routeIDs(rt), func(id string) bool { return slices.Contains(u.Routes, id) })
	})
	return retired
}

// heldCloseSession holds its Close until release closes; entered closes when
// Close is called.
type heldCloseSession struct {
	roleFakeSession
	entered chan struct{}
	release chan struct{}
}

func (s *heldCloseSession) Close(context.Context) error {
	close(s.entered)
	<-s.release
	return nil
}

// TestRetire_KeepsASessionARetiringRouteUsesOpen pins that a session no manager
// runs stays open while any route added with it may still run, retiring or
// not: retiring the second of two hand-wired routes sharing it, while the
// first's Retire is still draining, leaves it open, and the Retire that
// finishes last closes it once.
func TestRetire_KeepsASessionARetiringRouteUsesOpen(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	rt := New(WithInstanceID("retire-overlap-session"))
	common, recv1 := newRetireSession(), newComponentReceiver()
	require.NoError(t, rt.AddRoute(componentRoute("r1"), recv1, &componentSender{release: release}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), newComponentReceiver(), &componentSender{}, common, nil))
	startComponentRuntime(t, rt)
	first := retireHeldInDrain(t, rt, recv1, Unit{Routes: []string{"r1"}})

	retire(t, rt, Unit{Routes: []string{"r2"}})

	assert.Zero(t, common.closes.Load(), "a session a still-retiring route uses stays open")
	releaseOnce()
	require.NoError(t, wait.RequireReceive(t, first, 5*time.Second))
	assert.Equal(t, int32(1), common.closes.Load(), "the Retire that finishes last closes the session once")
	stopRuntime(t, rt)
	assert.Equal(t, int32(1), common.closes.Load(), "Stop does not close a session Retire closed")
}

// TestRetire_ARetireThatStoppedLetsGoOfASharedSession pins that a unit stops
// holding its sessions once its components have stopped, even before its
// Retire returns: the first of two overlapping Retires leaves the shared
// session to the second, whose route has not stopped, and the second, which
// still finds the first retiring while it closes a session of its own, closes
// the shared one once instead of both leaving it open.
func TestRetire_ARetireThatStoppedLetsGoOfASharedSession(t *testing.T) {
	release1, release2 := make(chan struct{}), make(chan struct{})
	own := &heldCloseSession{roleFakeSession: roleFakeSession{events: make(chan ports.SessionEvent, 1)},
		entered: make(chan struct{}), release: make(chan struct{})}
	releaseFirst := sync.OnceFunc(func() { close(release1) })
	releaseSecond := sync.OnceFunc(func() { close(release2) })
	releaseOwn := sync.OnceFunc(func() { close(own.release) })
	t.Cleanup(releaseFirst)
	t.Cleanup(releaseSecond)
	t.Cleanup(releaseOwn)
	rt := New(WithInstanceID("retire-overlap-stopped"))
	common := newRetireSession()
	require.NoError(t, rt.AddRoute(componentRoute("r1"), stuckReceiver{release: release1}, &componentSender{}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), stuckReceiver{release: release2}, &componentSender{}, common, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r3"), newComponentReceiver(), &componentSender{}, own, nil))
	startComponentRuntime(t, rt)
	first := retireAsync(t, rt, Unit{Routes: []string{"r1", "r3"}})
	second := retireAsync(t, rt, Unit{Routes: []string{"r2"}})

	releaseFirst()
	wait.RequireClosed(t, own.entered, 2*time.Second)
	assert.Zero(t, common.closes.Load(), "the first Retire leaves a session the second still drains on")
	releaseSecond()
	require.NoError(t, wait.RequireReceive(t, second, 5*time.Second))
	releaseOwn()
	require.NoError(t, wait.RequireReceive(t, first, 5*time.Second))

	assert.Equal(t, int32(1), common.closes.Load(), "the second Retire closes the shared session once")
}
