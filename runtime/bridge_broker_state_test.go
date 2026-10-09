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

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
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

// TestAskReceiversToEndBrokerState_SkipsAReceiverWhoseSessionNoManagerRuns pins
// that a receiver reading through an ending session no manager runs is not
// asked: nothing tells whether this instance may end that session's state.
//
// Mutation check: ask when managers has no manager for the session and this
// test fails.
func TestAskReceiversToEndBrokerState_SkipsAReceiverWhoseSessionNoManagerRuns(t *testing.T) {
	events := &brokerStateEvents{}
	recv := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	entries := []*routeEntry{{config: ridingRoute("r1", "s1"), receiver: recv}}

	askReceiversToEndBrokerState(t.Context(), entries, map[string]bool{"s1": true}, map[string]*session.Manager{})

	assert.False(t, events.ended())
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

// startUnsettledDelivery starts a runtime whose route r1 reads through a
// receiver rx on session s1, both able to end their broker state, and holds
// one delivery in flight on r1 that never settles: its send returns only when
// the route's run is cancelled, after a drain budget far shorter than the
// teardown's.
func startUnsettledDelivery(t *testing.T, events *brokerStateEvents, metrics ports.MetricsExporter) (*Runtime, *ackDelivery) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rt := New(WithInstanceID("broker-state-unsettled"), WithStopQuiesce(20*time.Millisecond), WithMetrics(metrics))
	s1 := newEndingSession("s1", events)
	recv := &endingReceiver{componentReceiver: newComponentReceiver(), id: "rx", events: events}
	require.NoError(t, rt.RegisterSessionSender(session.Config{SessionID: "s1"}, s1, nopRouteSender{}))
	require.NoError(t, rt.AddRoute(ridingRoute("r1", "s1"), recv, &componentSender{release: release}, s1, nil))
	startComponentRuntime(t, rt)
	wait.RequireClosed(t, recv.ready, 2*time.Second)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "unsettled", Subject: "broker-state"})
	del := &ackDelivery{syntheticDelivery: syntheticDelivery{env: env}}
	// Emitted under the receiver's run context, as a transport does, so the
	// cancel after the drain ends the send and leaves the delivery unsettled.
	require.NoError(t, recv.emit(recv.ctx, del))
	wait.Until(t, 2*time.Second, "the delivery is in flight", rt.anyRouteInFlight)
	return rt, del
}

// assertBrokerStateKeptForS1 asserts that neither rx nor s1 was asked to end
// its broker state, and that the skipped end of s1 was counted.
func assertBrokerStateKeptForS1(t *testing.T, events *brokerStateEvents, metrics *ports.RecordingExporter, del *ackDelivery) {
	t.Helper()
	assert.False(t, del.acked.Load(), "the delivery must still be unsettled for the test to hold")
	assert.Equal(t, []string{"close:rx", "close:s1"}, events.list(),
		"ending the state would delete the only copy of the unsettled delivery")
	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "s1"})
}

// TestRetire_EndsNoBrokerStateWhenTheDrainDidNotSettle pins that a retire whose
// drain did not settle every delivery before the cancel asks neither the
// receiver nor the session to end its broker state: the broker holds the only
// copy of the delivery the cancel left unsettled. The retire goes on as it
// does for any unsettled drain.
//
// Mutation check: drop the settled gate in Retire and this test fails.
func TestRetire_EndsNoBrokerStateWhenTheDrainDidNotSettle(t *testing.T) {
	events, metrics := &brokerStateEvents{}, &ports.RecordingExporter{}
	rt, del := startUnsettledDelivery(t, events, metrics)

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	assertBrokerStateKeptForS1(t, events, metrics, del)
}

// TestStopEndingBrokerState_EndsNoBrokerStateWhenTheDrainDidNotSettle pins the
// same for a full swap's stop.
//
// Mutation check: drop the settled gate in stop and this test fails.
func TestStopEndingBrokerState_EndsNoBrokerStateWhenTheDrainDidNotSettle(t *testing.T) {
	events, metrics := &brokerStateEvents{}, &ports.RecordingExporter{}
	rt, del := startUnsettledDelivery(t, events, metrics)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, rt.StopEndingBrokerState(ctx, []string{"s1"}))

	assertBrokerStateKeptForS1(t, events, metrics, del)
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
