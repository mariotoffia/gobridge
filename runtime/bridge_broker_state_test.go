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
// ("release:<id>") a test observes.
type brokerStateEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *brokerStateEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
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

func (s *endingSession) EndBrokerStateOnClose() { s.events.add("end:" + s.id) }

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

func (r *endingReceiver) EndBrokerStateOnClose() { r.events.add("end:" + r.id) }

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

	retire(t, rt, Unit{Routes: []string{"r1"}, Sessions: []string{"s1"}, EndBrokerState: []string{"s1"}})

	assert.Equal(t, []string{"end:rx", "close:rx"}, events.list())
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
	ender.EndBrokerStateOnClose()

	assert.Equal(t, []string{"end:rx"}, events.list())
}
