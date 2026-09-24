package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// componentReceiver hands the test the emit function its route runner passes
// to Run, and the context it runs under; ready closes once the runner is
// receiving.
type componentReceiver struct {
	ready chan struct{}
	ctx   context.Context
	emit  func(context.Context, ports.Delivery) error
}

func newComponentReceiver() *componentReceiver {
	return &componentReceiver{ready: make(chan struct{})}
}

func (r *componentReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.ctx, r.emit = ctx, emit
	close(r.ready)
	<-ctx.Done()
	return ctx.Err()
}

// deliver emits one message through the route once its runner is receiving.
func (r *componentReceiver) deliver(t *testing.T, id string) {
	t.Helper()
	wait.RequireClosed(t, r.ready, 2*time.Second)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: "component"})
	require.NoError(t, r.emit(context.Background(), &syntheticDelivery{env: env}))
}

// componentSender counts sends. A non-nil release holds every send until it is
// closed, which keeps a delivery in flight on demand.
type componentSender struct {
	release <-chan struct{}
	sent    atomic.Int32
}

func (s *componentSender) Send(ctx context.Context, _ ports.OutboundMessage) error {
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.sent.Add(1)
	return nil
}

// barrierSession accepts the settlement barrier the runtime installs, so the
// test can invoke it the way a transport does before recycling a connection.
type barrierSession struct {
	*roleFakeSession
	mu     sync.Mutex
	waiter func(context.Context) error
}

func newBarrierSession() *barrierSession {
	return &barrierSession{roleFakeSession: &roleFakeSession{events: make(chan ports.SessionEvent, 1)}}
}

func (s *barrierSession) SetIngressQuiescenceWaiter(waiter func(context.Context) error) {
	s.mu.Lock()
	s.waiter = waiter
	s.mu.Unlock()
}

func (s *barrierSession) waitIngressQuiescent(ctx context.Context) error {
	s.mu.Lock()
	waiter := s.waiter
	s.mu.Unlock()
	if waiter == nil {
		return errors.New("the runtime installed no settlement barrier on the session")
	}
	return waiter(ctx)
}

func componentRoute(id string) RouteConfig {
	return RouteConfig{
		ID: id,
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxInFlight:        4,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
}

func startComponentRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(ctx)
	})
}

func routeRunAt(rt *Runtime, i int) componentRun {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.entries[i].run
}

// TestStartComponent_CancelStopsOnlyThatComponent pins that every route runner
// runs under its own context: cancelling one route's run ends that route alone,
// as a clean stop, while its neighbour keeps delivering and the runtime stays
// healthy.
func TestStartComponent_CancelStopsOnlyThatComponent(t *testing.T) {
	rt := New(WithInstanceID("component-cancel"))
	recv1, recv2 := newComponentReceiver(), newComponentReceiver()
	sender2 := &componentSender{}
	require.NoError(t, rt.AddRoute(componentRoute("route-1"), recv1, &componentSender{}, nil, nil))
	require.NoError(t, rt.AddRoute(componentRoute("route-2"), recv2, sender2, nil, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv1.ready, 2*time.Second)

	first, second := routeRunAt(rt, 0), routeRunAt(rt, 1)
	first.cancel()
	wait.RequireClosed(t, first.done, 2*time.Second)
	assert.False(t, isClosed(second.done), "cancelling one route's run must not stop another route")

	recv2.deliver(t, "after-neighbour-stopped")
	wait.Until(t, 2*time.Second, "route-2 delivers after route-1 stopped", func() bool {
		return sender2.sent.Load() == 1
	})
	assert.True(t, rt.Healthy(), "a component stopped alone is a clean stop, not a failure")
	assert.False(t, rt.Terminal())
	assert.Empty(t, rt.ComponentErrors())
}

// TestDLQToken_AllowsLeaselessAndFencesExclusiveOnLocalLease pins the four
// rules the DLQ router's token function applies per owning session: a write
// with no owning session or for a session that carries no lease is allowed, a
// write for an exclusive session managed here is gated on that manager's
// lease, and a write for an exclusive session this instance does not manage is
// refused so its owner writes it.
func TestDLQToken_AllowsLeaselessAndFencesExclusiveOnLocalLease(t *testing.T) {
	rt := New(WithInstanceID("dlq-token"), WithLeaseStore(&roleGrantingLeaseStore{}))
	sess := &roleFakeSession{events: make(chan ports.SessionEvent, 1)}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true}, sess, nopRouteSender{}))
	startComponentRuntime(t, rt)

	rt.mu.Lock()
	mgr := rt.sessionMgrs["s1"]
	rt.mu.Unlock()
	require.NotNil(t, mgr)
	wait.Until(t, 2*time.Second, "s1 manager acquires its lease", func() bool {
		_, held := mgr.Token()
		return held
	})

	want, _ := mgr.Token()
	got, held := rt.dlqToken("s1")
	assert.True(t, held, "an exclusive session managed here reports its manager's lease")
	assert.Equal(t, want, got)

	for _, sid := range []string{"", "not-exclusive"} {
		tok, held := rt.dlqToken(sid)
		assert.True(t, held, "session %q carries no lease to fence on", sid)
		assert.Equal(t, persistence.LeaseToken{}, tok)
	}

	// An exclusive session whose manager lives on another instance.
	rt.mu.Lock()
	rt.exclusiveSessions["owned-elsewhere"] = true
	rt.mu.Unlock()
	_, held = rt.dlqToken("owned-elsewhere")
	assert.False(t, held, "an exclusive session not managed here must not authorize a DLQ write")
}

// TestSettlementBarrier_WaitsOnItsOwnRouteRunners pins that the settlement
// barrier a session gets waits on the route runners it was installed for, not on
// whichever runner currently carries that route id. A reload that retires a
// route and starts its successor under the same id must not let the retired
// session recycle its connection while its own delivery is still in flight.
func TestSettlementBarrier_WaitsOnItsOwnRouteRunners(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	rt := New(WithInstanceID("settlement-barrier"))
	recv := newComponentReceiver()
	sess := newBarrierSession()
	sessCfg := session.Config{SessionID: "source-session"}
	require.NoError(t, rt.AddRoute(componentRoute("source-route"), recv, &componentSender{release: release}, sess, &sessCfg))
	startComponentRuntime(t, rt)
	t.Cleanup(releaseOnce)

	recv.deliver(t, "held")
	rt.mu.Lock()
	original := rt.entries[0]
	rt.mu.Unlock()
	wait.Until(t, 2*time.Second, "the delivery is in flight", func() bool {
		return original.runner.InFlight() == 1
	})

	// An idle successor takes the route id, as a reload would leave it.
	rt.mu.Lock()
	rt.entries[0] = &routeEntry{
		config: original.config,
		runner: route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
			RouteID:  original.config.ID,
			Policy:   original.config.Policy,
			Receiver: blockingReceiver{},
			Sender:   nopRouteSender{},
		}),
	}
	rt.mu.Unlock()
	t.Cleanup(func() {
		rt.mu.Lock()
		rt.entries[0] = original
		rt.mu.Unlock()
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sess.waitIngressQuiescent(cancelled), context.Canceled,
		"the barrier must keep waiting while its own route's delivery is in flight")

	releaseOnce()
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer settleCancel()
	require.NoError(t, sess.waitIngressQuiescent(settleCtx))
}
