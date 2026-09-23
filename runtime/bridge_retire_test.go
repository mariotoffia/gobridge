package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// retireSession is a no-op session that counts its closes; closed closes on
// the first one.
type retireSession struct {
	roleFakeSession
	closes atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func newRetireSession() *retireSession {
	return &retireSession{
		roleFakeSession: roleFakeSession{events: make(chan ports.SessionEvent, 1)},
		closed:          make(chan struct{}),
	}
}

func (s *retireSession) Close(context.Context) error {
	s.closes.Add(1)
	s.once.Do(func() { close(s.closed) })
	return nil
}

// retireLeaseStore grants every lease, and records the leases released and how
// often a lease owner is looked up.
type retireLeaseStore struct {
	graftLeaseStore
	lookups  atomic.Int32
	relMu    sync.Mutex
	released []string
}

func (s *retireLeaseStore) Release(_ context.Context, leaseID string, _ persistence.LeaseToken) error {
	s.relMu.Lock()
	defer s.relMu.Unlock()
	s.released = append(s.released, leaseID)
	return nil
}

func (s *retireLeaseStore) Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.lookups.Add(1)
	return s.graftLeaseStore.Current(ctx, leaseID)
}

func (s *retireLeaseStore) releasedIDs() []string {
	s.relMu.Lock()
	defer s.relMu.Unlock()
	return slices.Clone(s.released)
}

// ackDelivery records whether the route acknowledged it.
type ackDelivery struct {
	syntheticDelivery
	acked atomic.Bool
}

func (d *ackDelivery) Ack(context.Context) error { d.acked.Store(true); return nil }

// ridingRoute is a direct_hold route whose receiver rides on session sid.
func ridingRoute(id, sid string) RouteConfig {
	cfg := componentRoute(id)
	cfg.SourceSessionID = sid
	return cfg
}

func retire(t *testing.T, rt *Runtime, u Unit) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rt.Retire(ctx, u))
}

// TestRetire_StopsOnlyTheNamedUnit pins that Retire stops and removes the unit
// it names while a route and session outside it keep running untouched.
func TestRetire_StopsOnlyTheNamedUnit(t *testing.T) {
	rt := New(WithInstanceID("retire-unit"))
	s1, s2 := newRetireSession(), newRetireSession()
	recv2, sender2 := newComponentReceiver(), &componentSender{}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s2"}, s2, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), newComponentReceiver(), &componentSender{}, s1, nil))
	require.NoError(t, rt.AddRoute(ridingRoute("r2", "s2"), recv2, sender2, s2, nil))
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})

	assert.Equal(t, int32(1), s1.closes.Load(), "the retired session is closed once")
	assert.Zero(t, s2.closes.Load(), "a session outside the unit stays open")
	assert.Equal(t, []string{"r2"}, routeIDs(rt))
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "late", Subject: "retire"})
	assert.ErrorIs(t, rt.Inject(context.Background(), "r1", env), shared.ErrNotFound)
	recv2.deliver(t, "after-retire")
	wait.Until(t, 2*time.Second, "the route outside the unit still delivers", func() bool {
		return sender2.sent.Load() == 1
	})
}

// TestRetire_SettlesInFlightDeliveryBeforeClosingSession pins that Retire lets
// an accepted delivery finish its send and settle before it stops the route and
// closes the session the route rides on, as Stop does for the whole runtime.
func TestRetire_SettlesInFlightDeliveryBeforeClosingSession(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	rt := New(WithInstanceID("retire-settle"))
	s1, recv := newRetireSession(), newComponentReceiver()
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{release: release}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "in-flight", Subject: "retire"})
	del := &ackDelivery{syntheticDelivery: syntheticDelivery{env: env}}
	// Emitted under the receiver's run context, as a transport does, so
	// stopping the route would cancel the send.
	require.NoError(t, recv.emit(recv.ctx, del))

	retired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		retired <- rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})
	}()
	wait.Until(t, 2*time.Second, "Retire takes the unit out", func() bool { return len(rt.Routes()) == 0 })
	wait.Silent(t, s1.closed, 25*time.Millisecond)
	releaseOnce()

	require.NoError(t, wait.RequireReceive(t, retired, 5*time.Second))
	assert.True(t, del.acked.Load(), "the in-flight delivery settles before its route stops")
	assert.Equal(t, int32(1), s1.closes.Load())
}

// TestRetire_ReleasesExclusiveLease pins that retiring an exclusive session
// hands its lease back, so the successor, here or on a peer, can take it.
func TestRetire_ReleasesExclusiveLease(t *testing.T) {
	leases := &retireLeaseStore{}
	rt := New(WithInstanceID("retire-lease"), WithLeaseStore(leases))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true}, newRetireSession(), nopRouteSender{}))
	startComponentRuntime(t, rt)
	wait.Until(t, 2*time.Second, "s1 acquires its lease", func() bool { return rt.LeaseStatus()["s1"] })

	retire(t, rt, Unit{Sessions: []string{"s1"}})

	assert.Equal(t, []string{"s1"}, leases.releasedIDs(), "the retired session releases its lease")
	assert.NotContains(t, rt.LeaseStatus(), "s1")
}

// TestRetireThenGraft_ExclusiveSessionReacquiresLeaseAndFencesDLQ pins the
// reload of an exclusive unit: once Retire returns, a successor with the same
// route and session ids grafts cleanly, its new manager acquires the lease,
// and DLQ writes for the session are fenced on that manager's lease.
func TestRetireThenGraft_ExclusiveSessionReacquiresLeaseAndFencesDLQ(t *testing.T) {
	stores := newGraftStores()
	host := New(stores.options(WithInstanceID("retire-then-graft"))...)
	cfg := session.DefaultConfig("s1", true)
	require.NoError(t, host.AddRoute(outboxRoute("r1"), newComponentReceiver(), &componentSender{}, newRetireSession(), &cfg))
	startComponentRuntime(t, host)
	wait.Until(t, 2*time.Second, "s1 acquires its lease", func() bool { return host.LeaseStatus()["s1"] })
	host.mu.Lock()
	oldMgr, oldDrainer := host.sessionMgrs["s1"], host.drainers[0]
	host.mu.Unlock()

	retire(t, host, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})
	assert.True(t, isClosed(oldDrainer.run.done), "the retired session's drainer has stopped")

	part := New(stores.options(WithSharedStores())...)
	partCfg := session.DefaultConfig("s1", true)
	require.NoError(t, part.AddRoute(outboxRoute("r1"), newComponentReceiver(), &componentSender{}, newRetireSession(), &partCfg))
	require.NoError(t, host.Graft(part))
	wait.Until(t, 2*time.Second, "the successor acquires the lease", func() bool { return host.LeaseStatus()["s1"] })

	host.mu.Lock()
	mgr, drainers := host.sessionMgrs["s1"], slices.Clone(host.drainers)
	host.mu.Unlock()
	require.NotSame(t, oldMgr, mgr)
	want, _ := mgr.Token()
	got, held := host.dlqToken("s1")
	assert.True(t, held, "a DLQ write for the successor is fenced on its lease")
	assert.Equal(t, want, got)
	require.Len(t, drainers, 1)
	assert.NotSame(t, oldDrainer, drainers[0])
}

// TestRetireThenGraft_NonExclusiveSuccessorIsNotFenced pins that Retire clears
// a retired session's exclusive mark: a successor under the same id that holds
// no lease must not have its DLQ writes refused for want of one.
func TestRetireThenGraft_NonExclusiveSuccessorIsNotFenced(t *testing.T) {
	stores := newGraftStores()
	host := New(stores.options(WithInstanceID("retire-unfenced"))...)
	require.NoError(t, host.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true}, newRetireSession(), nopRouteSender{}))
	startComponentRuntime(t, host)
	retire(t, host, Unit{Sessions: []string{"s1"}})

	part := New(stores.options(WithSharedStores())...)
	require.NoError(t, part.RegisterSessionSender(session.Config{SessionID: "s1"}, newRetireSession(), nopRouteSender{}))
	require.NoError(t, host.Graft(part))

	_, held := host.dlqToken("s1")
	assert.True(t, held, "a session without a lease is not fenced")
}

// TestRetire_ClearsTheRetiredRoutesFaults pins that a fault a retired route's
// supervisor recorded does not outlive the route, where it would mark a
// successor under the same id failed.
func TestRetire_ClearsTheRetiredRoutesFaults(t *testing.T) {
	rt := New(WithInstanceID("retire-faults"))
	require.NoError(t, rt.AddRoute(componentRoute("r1"), failingReceiver{}, &componentSender{}, nil, nil))
	startComponentRuntime(t, rt)
	wait.Until(t, 2*time.Second, "the route records its fault", func() bool {
		return rt.ComponentErrors()["route:r1"] != nil
	})

	retire(t, rt, Unit{Routes: []string{"r1"}})

	assert.NotContains(t, rt.ComponentErrors(), "route:r1")
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.NotContains(t, rt.routeFlaps, "route:r1")
}

// failingReceiver fails every run, so its route's supervisor records the fault
// and backs off.
type failingReceiver struct{}

func (failingReceiver) Run(context.Context, func(context.Context, ports.Delivery) error) error {
	return errors.New("source unavailable")
}

// TestRetire_ReportsComponentsThatDidNotStop pins that a component that does
// not stop within ctx is reported, while the unit still leaves the runtime and
// its session is still closed, so its lease is handed back.
func TestRetire_ReportsComponentsThatDidNotStop(t *testing.T) {
	rt := New(WithInstanceID("retire-stuck"))
	s1, release := newRetireSession(), make(chan struct{})
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), stuckReceiver{release: release}, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})

	require.ErrorContains(t, err, "did not finish")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, routeIDs(rt))
	assert.Equal(t, int32(1), s1.closes.Load())
}

// stuckReceiver ignores cancellation until released.
type stuckReceiver struct{ release <-chan struct{} }

func (r stuckReceiver) Run(context.Context, func(context.Context, ports.Delivery) error) error {
	<-r.release
	return nil
}

// TestRetire_UnregistersExclusiveRouteFromLocator pins that a retired exclusive
// route is no longer located through its session's lease.
func TestRetire_UnregistersExclusiveRouteFromLocator(t *testing.T) {
	leases := &retireLeaseStore{}
	rt := New(WithInstanceID("retire-locator"), WithLeaseStore(leases),
		WithOutboxStore(&graftOutboxStore{}), WithDLQStore(&graftDLQStore{}))
	cfg := session.DefaultConfig("s1", true)
	require.NoError(t, rt.AddRoute(outboxRoute("r1"), newComponentReceiver(), &componentSender{}, newRetireSession(), &cfg))
	startComponentRuntime(t, rt)
	locator := rt.RouteLocator()
	_, _, _ = locator.Locate(context.Background(), "r1")
	lookups := leases.lookups.Load()
	require.Positive(t, lookups, "precondition: an exclusive route's owner is looked up")

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})

	_, local, err := locator.Locate(context.Background(), "r1")
	require.NoError(t, err)
	assert.True(t, local)
	assert.Equal(t, lookups, leases.lookups.Load(), "a retired route is located without a lease lookup")
}

// TestRetire_ForgetsCredentialTargets pins that Retire tells every credential
// refresher to stop watching the retired transports, and closes and drops a
// refresher that is left watching nothing, so Stop does not close it again.
func TestRetire_ForgetsCredentialTargets(t *testing.T) {
	rt := New(WithInstanceID("retire-credentials"))
	s1, recv1, sender1, recv2 := newRetireSession(), newComponentReceiver(), &componentSender{}, newComponentReceiver()
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv1, sender1, s1, nil))
	require.NoError(t, rt.AddRoute(componentRoute("r2"), recv2, &componentSender{}, nil, nil))
	var idleCloses, busyCloses atomic.Int32
	var forgotten []any
	rt.AttachCredentialCloser(func(context.Context) { idleCloses.Add(1) })
	rt.AttachCredentialForget(func(targets []any) bool { forgotten = targets; return true })
	rt.AttachCredentialCloser(func(context.Context) { busyCloses.Add(1) })
	rt.AttachCredentialForget(func([]any) bool { return false })
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}})

	for name, target := range map[string]any{"session": s1, "receiver": recv1, "sender": sender1} {
		assert.True(t, slices.Contains(forgotten, target), "the retired %s is forgotten", name)
	}
	assert.False(t, slices.Contains(forgotten, any(recv2)), "a route outside the unit keeps its credentials")
	assert.Equal(t, int32(1), idleCloses.Load(), "a refresher left watching nothing is closed")
	assert.Zero(t, busyCloses.Load(), "a refresher still watching a live transport stays open")

	stopRuntime(t, rt)
	assert.Equal(t, int32(1), idleCloses.Load(), "Stop does not close a refresher Retire closed")
	assert.Equal(t, int32(1), busyCloses.Load())
}

// TestRetire_ClosesTheSessionsOnlyTheUnitHeld pins which sessions Retire closes
// besides its managers': a session no manager runs that only the unit held is
// closed once, and a session a surviving manager runs is left to that manager.
// The unit breaks Retire's closed-over-its-sessions precondition on purpose —
// it names r2 but not the s2 it rides on — to pin that safety net: Retire never
// closes a session under a manager it leaves running.
func TestRetire_ClosesTheSessionsOnlyTheUnitHeld(t *testing.T) {
	rt := New(WithInstanceID("retire-unmanaged"))
	bare, s2 := newRetireSession(), newRetireSession()
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s2"}, s2, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(componentRoute("r1"), newComponentReceiver(), &componentSender{}, bare, nil))
	require.NoError(t, rt.AddRoute(ridingRoute("r2", "s2"), newComponentReceiver(), &componentSender{}, s2, nil))
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Routes: []string{"r1", "r2"}})

	assert.Equal(t, int32(1), bare.closes.Load(), "a session only the unit held is closed")
	assert.Zero(t, s2.closes.Load(), "a session whose manager keeps running stays open")
	stopRuntime(t, rt)
	assert.Equal(t, int32(1), bare.closes.Load(), "Stop does not close a session Retire closed")
	assert.Equal(t, int32(1), s2.closes.Load())
}

// TestRetire_RetiresAnIngressSession pins that an ingress session retires with
// the route riding on it: its manager closes it once, a credential refresher
// forgets it, and another ingress session stays.
func TestRetire_RetiresAnIngressSession(t *testing.T) {
	rt := New(WithInstanceID("retire-ingress"))
	i1, i2 := newRetireSession(), newRetireSession()
	require.NoError(t, rt.RegisterIngressSession(session.Config{SessionID: "i1"}, i1))
	require.NoError(t, rt.RegisterIngressSession(session.Config{SessionID: "i2"}, i2))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "i1"), newComponentReceiver(), &componentSender{}, nil, nil))
	var forgotten []any
	rt.AttachCredentialForget(func(targets []any) bool { forgotten = targets; return false })
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"i1"}})

	assert.Equal(t, int32(1), i1.closes.Load(), "the retired ingress session is closed once")
	assert.Zero(t, i2.closes.Load())
	assert.True(t, slices.Contains(forgotten, any(i1)), "the retired ingress session is forgotten")
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.NotContains(t, rt.ingressSessions, "i1")
	assert.NotContains(t, rt.sessionMgrs, "i1")
	assert.Contains(t, rt.sessionMgrs, "i2")
	assert.Empty(t, rt.retiring, "a finished Retire leaves nothing behind")
}

// TestRetire_OnStoppedRuntimeReturnsError pins that only a running runtime
// retires anything: a refused Retire closes nothing.
func TestRetire_OnStoppedRuntimeReturnsError(t *testing.T) {
	cases := map[string]func(t *testing.T, rt *Runtime){
		"never started": func(*testing.T, *Runtime) {},
		"stopped": func(t *testing.T, rt *Runtime) {
			require.NoError(t, rt.Start(context.Background()))
			stopRuntime(t, rt)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			rt := New()
			s1 := newRetireSession()
			require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
			prepare(t, rt)
			closes := s1.closes.Load()

			require.ErrorContains(t, rt.Retire(context.Background(), Unit{Sessions: []string{"s1"}}), "runtime is not running")

			assert.Equal(t, closes, s1.closes.Load(), "a refused Retire closes nothing")
		})
	}
}

// TestRetire_UnknownIDsAreIgnored pins that ids the runtime does not have are
// no error and touch nothing.
func TestRetire_UnknownIDsAreIgnored(t *testing.T) {
	rt := New(WithInstanceID("retire-unknown"))
	s1 := newRetireSession()
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), newComponentReceiver(), &componentSender{}, s1, nil))
	var forgets atomic.Int32
	rt.AttachCredentialForget(func([]any) bool { forgets.Add(1); return true })
	startComponentRuntime(t, rt)
	run := routeRunAt(rt, 0)

	retire(t, rt, Unit{Routes: []string{"no-such-route"}, Sessions: []string{"no-such-session"}})

	assert.Zero(t, s1.closes.Load())
	assert.Equal(t, []string{"r1"}, routeIDs(rt))
	assert.False(t, isClosed(run.done), "the runtime's own route keeps running")
	assert.Zero(t, forgets.Load(), "with nothing retired no refresher is asked to forget")
}
