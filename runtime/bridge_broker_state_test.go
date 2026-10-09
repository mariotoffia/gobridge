package runtime

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// brokerStateEvents records, in order, the asks to end broker state
// ("end:<id>"), the closes ("close:<id>") and the lease releases
// ("release:<id>") a test observes, and the deadline each ask carried.
type brokerStateEvents struct {
	mu     sync.Mutex
	events []string
	before map[string]time.Time
}

func (e *brokerStateEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

// ask records that id was asked to end its broker state by before.
func (e *brokerStateEvents) ask(id string, before time.Time) {
	e.add("end:" + id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.before == nil {
		e.before = map[string]time.Time{}
	}
	e.before[id] = before
}

// askedBefore is the deadline id was last asked to end its broker state by.
func (e *brokerStateEvents) askedBefore(id string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.before[id]
}

func (e *brokerStateEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

func (e *brokerStateEvents) ended() bool {
	return slices.ContainsFunc(e.list(), func(event string) bool { return strings.HasPrefix(event, "end:") })
}

// endingSession is a session whose broker state can be ended.
type endingSession struct {
	roleFakeSession
	id     string
	events *brokerStateEvents
}

func newEndingSession(id string, events *brokerStateEvents) *endingSession {
	return &endingSession{roleFakeSession: roleFakeSession{events: make(chan ports.SessionEvent, 1)}, id: id, events: events}
}

func (s *endingSession) EndBrokerStateOnClose(before time.Time) { s.events.ask(s.id, before) }

func (s *endingSession) Close(context.Context) error {
	s.events.add("close:" + s.id)
	return nil
}

// endingReceiver is a receiver whose broker state can be ended.
type endingReceiver struct {
	*componentReceiver
	id     string
	events *brokerStateEvents
}

func (r *endingReceiver) EndBrokerStateOnClose(before time.Time) { r.events.ask(r.id, before) }

func (r *endingReceiver) Close(context.Context) error {
	r.events.add("close:" + r.id)
	return nil
}

// eventLeaseStore grants every lease and records each release.
type eventLeaseStore struct {
	graftLeaseStore
	events *brokerStateEvents
}

func (s *eventLeaseStore) Release(_ context.Context, leaseID string, _ persistence.LeaseToken) error {
	s.events.add("release:" + leaseID)
	return nil
}

var (
	_ ports.BrokerStateEnder = (*endingSession)(nil)
	_ ports.BrokerStateEnder = (*endingReceiver)(nil)
)

// startExclusiveEndingSession starts a runtime over leases with one exclusive
// session sid, and waits until it holds the lease.
func startExclusiveEndingSession(t *testing.T, sid string, events *brokerStateEvents) *Runtime {
	t.Helper()
	rt := New(WithInstanceID("broker-state"), WithLeaseStore(&eventLeaseStore{events: events}))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: sid, Exclusive: true}, newEndingSession(sid, events), nopRouteSender{}))
	startComponentRuntime(t, rt)
	wait.Until(t, 2*time.Second, sid+" acquires its lease", func() bool { return rt.LeaseStatus()[sid] })
	return rt
}

func TestRetire_EndsTheBrokerStateOfALostSessionBeforeReleasingItsLease(t *testing.T) {
	events := &brokerStateEvents{}
	rt := startExclusiveEndingSession(t, "s1", events)

	retire(t, rt, Unit{Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	assert.Equal(t, []string{"end:s1", "close:s1", "release:s1"}, events.list())
}

func TestRetire_EndsNoBrokerStateOfASessionTheNextConfigurationKeeps(t *testing.T) {
	events := &brokerStateEvents{}
	rt := startExclusiveEndingSession(t, "s1", events)

	retire(t, rt, Unit{Sessions: []string{"s1"}})

	assert.Equal(t, []string{"close:s1", "release:s1"}, events.list())
}

func TestRetire_IgnoresAnEndForASessionTheUnitDoesNotName(t *testing.T) {
	events := &brokerStateEvents{}
	rt := startExclusiveEndingSession(t, "s1", events)

	retire(t, rt, Unit{Sessions: []string{"s1"}, EndBrokerState: []string{"s2"}})

	assert.False(t, events.ended())
}

func TestRetire_EndsNoBrokerStateWithoutTheLease(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-standby"), WithLeaseStore(heldElsewhereLeaseStore{}))
	s1 := newEndingSession("s1", events)
	recv := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	assert.Equal(t, []string{"close:rx", "close:s1"}, events.list(),
		"a standby never connected as the identity, neither through the session nor a receiver reading through it")
}

func TestRetire_EndsTheBrokerStateOfASessionWithoutALease(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-no-lease"))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, newEndingSession("s1", events), nopRouteSender{}))
	startComponentRuntime(t, rt)

	retire(t, rt, Unit{Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	assert.Equal(t, []string{"end:s1", "close:s1"}, events.list())
}

func TestRetire_AsksAReceiverOnALostSessionToEndItsBrokerStateBeforeItCloses(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-receiver"))
	s1 := newRetireSession()
	recv := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)

	require.NoError(t, rt.Retire(t.Context(), Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}}))

	assert.Equal(t, []string{"end:rx", "close:rx"}, events.list())
	assert.Zero(t, events.askedBefore("rx"), "neither a session without a lease nor a retire without a deadline bounds the ending")
}

// TestRetire_AsksAReceiverToEndItsBrokerStateByItsSessionsLeaseDeadline pins
// that a receiver reading through a lease-managed session is asked to finish
// ending its broker state by the local deadline of that session's lease: past
// it a standby may own the identity.
//
// Mutation check: ask the receiver with a zero deadline in
// askReceiversToEndBrokerState and this test fails.
func TestRetire_AsksAReceiverToEndItsBrokerStateByItsSessionsLeaseDeadline(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-receiver-lease"), WithLeaseStore(&eventLeaseStore{events: events}))
	s1 := newEndingSession("s1", events)
	recv := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	// A lease far longer than the test keeps the deadline from being renewed.
	cfg := session.Config{SessionID: "s1", Exclusive: true, LeaseTTL: time.Hour}
	require.NoError(t, rt.RegisterSessionSender(cfg, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)
	wait.Until(t, 2*time.Second, "s1 acquires its lease", func() bool { return rt.LeaseStatus()["s1"] })
	rt.mu.Lock()
	mgr := rt.sessionMgrs["s1"]
	rt.mu.Unlock()
	deadline, ok := mgr.MayEndBrokerState()
	require.True(t, ok)
	require.False(t, deadline.IsZero())

	// A retire without a deadline of its own leaves the lease deadline to bound
	// the ending.
	require.NoError(t, rt.Retire(t.Context(), Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}}))

	assert.Equal(t, deadline, events.askedBefore("rx"))
	assert.Equal(t, deadline, events.askedBefore("s1"))
}

// unacknowledgedEndReceiver is a receiver whose closing detach the broker never
// acknowledges: once asked to end its broker state, its Close returns only when
// the deadline the ask carried passed, or its ctx is done.
type unacknowledgedEndReceiver struct {
	*componentReceiver
	mu     sync.Mutex
	before time.Time
}

func (r *unacknowledgedEndReceiver) EndBrokerStateOnClose(before time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = before
}

func (r *unacknowledgedEndReceiver) askedBefore() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.before
}

func (r *unacknowledgedEndReceiver) Close(ctx context.Context) error {
	if before := r.askedBefore(); !before.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, before)
		defer cancel()
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestRetire_AsksAReceiverToEndItsBrokerStateWithinTheRetireBudget pins that a
// receiver is asked to finish ending its broker state while the retire can
// still finish. The route runner closes the receiver inside the run the retire
// waits for, under a close budget of its own (ReceiverCloseTimeout, 10s by
// default) that outlasts this retire's; a closing detach the broker never
// acknowledges then gives up in time, and the reload goes on.
//
// Mutation check: ask the receiver by its session's lease deadline alone in
// askReceiversToEndBrokerState and the retire reports that its routes did not
// finish.
func TestRetire_AsksAReceiverToEndItsBrokerStateWithinTheRetireBudget(t *testing.T) {
	rt := New(WithInstanceID("broker-state-receiver-budget"))
	s1 := newRetireSession()
	recv := &unacknowledgedEndReceiver{componentReceiver: newComponentReceiver()}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	budgetEnd, _ := ctx.Deadline()
	require.NoError(t, rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}}))

	before := recv.askedBefore()
	require.False(t, before.IsZero(), "a retire with a deadline bounds the ending")
	assert.True(t, before.Before(budgetEnd), "the ending must give up before the retire's budget ends")
}

func TestRetire_EndsNoBrokerStateWhenAComponentDidNotStop(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-stuck"))
	s1, release := newEndingSession("s1", events), make(chan struct{})
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), stuckReceiver{release: release}, &componentSender{}, s1, nil))
	startComponentRuntime(t, rt)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := rt.Retire(ctx, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	require.ErrorContains(t, err, "did not finish")
	assert.Equal(t, []string{"close:s1"}, events.list(),
		"a session ends nothing while a component of its unit may still use it")
}

func TestStopEndingBrokerState_EndsOnlyTheNamedSessionsBeforeReleasingTheirLeases(t *testing.T) {
	events := &brokerStateEvents{}
	rt := New(WithInstanceID("broker-state-stop"), WithLeaseStore(&eventLeaseStore{events: events}))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1", Exclusive: true}, newEndingSession("s1", events), nopRouteSender{}))
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s2", Exclusive: true}, newEndingSession("s2", events), nopRouteSender{}))
	startComponentRuntime(t, rt)
	wait.Until(t, 2*time.Second, "both sessions acquire their leases", func() bool {
		status := rt.LeaseStatus()
		return status["s1"] && status["s2"]
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rt.StopEndingBrokerState(ctx, []string{"s1"}))

	got := events.list()
	assert.Contains(t, got, "end:s1")
	assert.Less(t, slices.Index(got, "end:s1"), slices.Index(got, "close:s1"))
	assert.Less(t, slices.Index(got, "close:s1"), slices.Index(got, "release:s1"))
	assert.NotContains(t, got, "end:s2", "a session whose key survives ends nothing")
	assert.Contains(t, got, "release:s2")
}

func TestStop_EndsNoBrokerState(t *testing.T) {
	events := &brokerStateEvents{}
	rt := startExclusiveEndingSession(t, "s1", events)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rt.Stop(ctx))

	assert.Equal(t, []string{"close:s1", "release:s1"}, events.list())
}

func TestInstrumentedReceiver_ForwardsTheAskToEndBrokerState(t *testing.T) {
	events := &brokerStateEvents{}
	inner := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	wrapped := NewInstrumentedReceiver(inner, &ports.NoopExporter{}, "ReceiveLatency", "route_id", "r1", nil)

	ender, ok := any(wrapped).(ports.BrokerStateEnder)
	require.True(t, ok, "a wrapper must not hide the capability from a retire")
	ender.EndBrokerStateOnClose(time.Time{})

	assert.Equal(t, []string{"end:rx"}, events.list())
}
