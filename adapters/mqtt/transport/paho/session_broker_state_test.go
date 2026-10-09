package paho

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

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// connectedSessionCountingEnds returns a session in mode that is connected,
// whose attempts to end its broker session are counted in ends and return err.
func connectedSessionCountingEnds(mode connectivity.SessionMode, ends *atomic.Int32, err error, metrics ...ports.MetricsExporter) *Session {
	s := NewSession(SessionOptions{ClientID: "orders-client"}, mode, nil, metrics...)
	s.endBrokerSessionOverride = func(context.Context) error {
		ends.Add(1)
		return err
	}
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()
	return s
}

// stepRecorder records the order of the steps a Close takes.
type stepRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *stepRecorder) add(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *stepRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// recordingDisconnectConn is a live connection whose Disconnect is recorded
// in steps and returns err.
type recordingDisconnectConn struct {
	fakeLiveConn
	steps *stepRecorder
	err   error
}

func (c *recordingDisconnectConn) Disconnect(context.Context) error {
	c.steps.add("disconnect")
	return c.err
}

func TestClose_EndsTheBrokerSessionWhenAskedWhileConnected(t *testing.T) {
	for _, mode := range []connectivity.SessionMode{connectivity.SessionPersistent, connectivity.SessionExclusive} {
		var ends atomic.Int32
		s := connectedSessionCountingEnds(mode, &ends, nil)
		s.EndBrokerStateOnClose(time.Time{})

		require.NoError(t, s.Close(t.Context()))

		assert.Equal(t, int32(1), ends.Load(), "mode %s", mode)
	}
}

func TestClose_EndsTheBrokerSessionAfterItsOwnConnectionClosed(t *testing.T) {
	steps := &stepRecorder{}
	s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil)
	s.endBrokerSessionOverride = func(context.Context) error {
		steps.add("end")
		return nil
	}
	s.mu.Lock()
	s.cm = &recordingDisconnectConn{steps: steps}
	s.connected = true
	s.mu.Unlock()
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()))

	assert.Equal(t, []string{"disconnect", "end"}, steps.list(),
		"a clean-start connection while the session is connected would take over its client ID")
}

func TestClose_EndsNothingWithoutTheAsk(t *testing.T) {
	var ends atomic.Int32
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil)

	require.NoError(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load(), "a shutdown, pause or rebuild keeps the broker session")
}

func TestClose_EndsNothingWhenNotConnected(t *testing.T) {
	for name, state := range map[string]struct {
		cm        pahoConnection
		connected bool
	}{
		"never connected": {},
		// autopaho lost the connection and is reconnecting as the client ID.
		"reconnecting": {cm: &fakeLiveConn{}},
		// The connection came up but its Start has not installed it yet.
		"start in flight": {connected: true},
	} {
		t.Run(name, func(t *testing.T) {
			var ends atomic.Int32
			s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil)
			s.endBrokerSessionOverride = func(context.Context) error {
				ends.Add(1)
				return nil
			}
			s.mu.Lock()
			s.cm = state.cm
			s.connected = state.connected
			s.mu.Unlock()
			s.EndBrokerStateOnClose(time.Time{})

			require.NoError(t, s.Close(t.Context()))

			assert.Zero(t, ends.Load(), "only an instance connected as the identity ends its state")
		})
	}
}

func TestClose_EndsNothingForAnEphemeralSession(t *testing.T) {
	var ends atomic.Int32
	s := connectedSessionCountingEnds(connectivity.SessionEphemeral, &ends, nil)
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load())
}

func TestClose_CountsAFailureToEndTheBrokerSessionAndStillCloses(t *testing.T) {
	var ends atomic.Int32
	metrics := &ports.RecordingExporter{}
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, errors.New("broker unreachable"), metrics)
	s.sessionID = "orders-session"
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()), "a failure to end broker state never fails the reload")

	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1)
	assert.Equal(t, int64(1), entries[0].IValue)
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
}

// TestClose_EndsTheBrokerSessionByTheLeaseDeadline pins that the clean-start
// connection runs under the deadline the ask carried, and that once that
// deadline passed nothing is dialled: another instance may own the client ID
// by then, and a clean-start connection would take it over and delete its
// broker session. Not dialling is a failure to end, counted as any other.
//
// Mutation check: drop the deadline from endBrokerStateAfterClose and the
// passed-deadline case dials.
func TestClose_EndsTheBrokerSessionByTheLeaseDeadline(t *testing.T) {
	t.Run("deadline ahead", func(t *testing.T) {
		before := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
		var got time.Time
		s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil)
		s.endBrokerSessionOverride = func(ctx context.Context) error {
			got, _ = ctx.Deadline()
			return nil
		}
		s.mu.Lock()
		s.cm = &fakeLiveConn{}
		s.connected = true
		s.mu.Unlock()
		s.EndBrokerStateOnClose(before)

		require.NoError(t, s.Close(t.Context()))

		assert.Equal(t, before, got)
	})

	t.Run("deadline passed", func(t *testing.T) {
		var ends atomic.Int32
		metrics := &ports.RecordingExporter{}
		s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil, metrics)
		s.sessionID = "orders-session"
		s.EndBrokerStateOnClose(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))

		require.NoError(t, s.Close(t.Context()))

		assert.Zero(t, ends.Load(), "past the lease deadline another instance may own the client ID")
		entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
		require.Len(t, entries, 1, "the broker session was not ended")
		assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
	})
}

func TestClose_DoesNotEndTheBrokerSessionWhenItsOwnDisconnectFailed(t *testing.T) {
	var ends atomic.Int32
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil, metrics)
	s.sessionID = "orders-session"
	s.endBrokerSessionOverride = func(context.Context) error {
		ends.Add(1)
		return nil
	}
	s.mu.Lock()
	s.cm = &recordingDisconnectConn{steps: &stepRecorder{}, err: context.DeadlineExceeded}
	s.connected = true
	s.mu.Unlock()
	s.EndBrokerStateOnClose(time.Time{})

	require.Error(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load(), "autopaho may still be connected or reconnecting as the client ID")
	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1, "the broker session was not ended")
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
}

// hookedDisconnectConn is a live connection that runs onDisconnect when Close
// disconnects it.
type hookedDisconnectConn struct {
	fakeLiveConn
	onDisconnect func()
}

func (c *hookedDisconnectConn) Disconnect(context.Context) error {
	c.onDisconnect()
	return nil
}

// TestClose_KeepsTheBrokerSessionWhileAReceivedDeliveryIsUnsettled pins that
// a session asked to end its broker session sends no clean-start connection
// while a QoS 1/2 delivery it received is unacknowledged. A delivery the
// runtime admitted after its drain check, and whose route was then cancelled,
// is never acknowledged: the broker session holds its only copy. The kept
// session is counted as a failure to end.
//
// Mutation check: drop the unsettled check from endBrokerStateAfterClose and
// the clean-start connection runs.
func TestClose_KeepsTheBrokerSessionWhileAReceivedDeliveryIsUnsettled(t *testing.T) {
	var ends atomic.Int32
	metrics := &ports.RecordingExporter{}
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil, metrics)
	s.sessionID = "orders-session"
	// A received QoS 1 publish whose acknowledgement the runtime never calls.
	_ = s.router.trackAcknowledgement(func() error { return nil })
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()), "keeping the broker session never fails the reload")

	assert.Zero(t, ends.Load(), "ending the broker session would delete the unsettled delivery")
	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1, "the broker session was not ended")
	assert.Equal(t, int64(1), entries[0].IValue)
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
}

func TestClose_EndsTheBrokerSessionOnceItsReceivedDeliveryIsAcknowledged(t *testing.T) {
	var ends atomic.Int32
	metrics := &ports.RecordingExporter{}
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil, metrics)
	ack := s.router.trackAcknowledgement(func() error { return nil })
	require.NoError(t, ack())
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()))

	assert.Equal(t, int32(1), ends.Load())
	assert.Empty(t, metrics.FindEntries(shared.MetricBrokerStateEndFailures))
}

// TestClose_KeepsTheBrokerSessionForADeliveryReceivedWhileClosing pins the second look at the
// unsettled record: a publish the client reads after Close stopped the router,
// and before its connection is down, is never acknowledged either.
//
// Mutation check: read the unsettled record only once, before the router
// stops, and the clean-start connection runs.
func TestClose_KeepsTheBrokerSessionForADeliveryReceivedWhileClosing(t *testing.T) {
	var ends atomic.Int32
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil)
	s.mu.Lock()
	s.cm = &hookedDisconnectConn{onDisconnect: func() {
		_ = s.router.trackAcknowledgement(func() error { return nil })
	}}
	s.mu.Unlock()
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load(), "the publish received while closing is unsettled")
}

// TestClose_KeepsTheBrokerSessionWhenARacingRecoveryClearsTheRecord pins the first look at the
// unsettled record: a settlement recovery racing the Close clears the record
// as it recycles the connection, yet the deliveries it held are still
// unacknowledged in the broker session the end step would delete.
//
// Mutation check: read the unsettled record only at the end step and the
// clean-start connection runs.
func TestClose_KeepsTheBrokerSessionWhenARacingRecoveryClearsTheRecord(t *testing.T) {
	var ends atomic.Int32
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, nil)
	_ = s.router.trackAcknowledgement(func() error { return nil })
	s.mu.Lock()
	s.cm = &hookedDisconnectConn{onDisconnect: func() {
		s.router.mu.Lock()
		s.router.clearUnsettledLocked()
		s.router.mu.Unlock()
	}}
	s.mu.Unlock()
	s.EndBrokerStateOnClose(time.Time{})

	require.NoError(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load(), "the cleared delivery is still unacknowledged on the broker")
}

func TestMetricSessionID_IsTheGoBridgeSessionIDOrElseTheClientID(t *testing.T) {
	s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil)
	assert.Equal(t, "orders-client", s.metricSessionID(), "a session built without the factory has no session_id")

	s.sessionID = "orders-session"
	assert.Equal(t, "orders-session", s.metricSessionID())
}

func TestHealth_TagsUnsettledMetricsWithTheSessionID(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil, metrics)
	s.sessionID = "orders-session"
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	_ = s.Health(t.Context())

	for _, name := range []string{MetricMQTTUnsettled, MetricMQTTOldestUnsettledAge, MetricMQTTReceiveWindowUtilization} {
		entries := metrics.FindEntries(name)
		require.NotEmpty(t, entries, name)
		assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"}, name)
	}
}

func TestFactoryNewSession_KnowsItsSessionID(t *testing.T) {
	raw, err := NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID:          "orders-session",
		SessionMode: connectivity.SessionPersistent,
		Config: &Config{Session: SessionOptions{
			BrokerURLs: []string{"tcp://192.0.2.1:1883"},
			ClientID:   "orders-client",
		}},
	})
	require.NoError(t, err)
	s, ok := raw.(*Session)
	require.True(t, ok)

	assert.Equal(t, "orders-session", s.metricSessionID())
}

// startOrder makes s record, in operations, each attempt to end its broker
// session ("end") and each dial ("dial").
func startOrder(s *Session, operations *[]string) {
	s.endBrokerSessionOverride = func(context.Context) error {
		*operations = append(*operations, "end")
		return nil
	}
	dial := s.connectOverride
	s.connectOverride = func(ctx context.Context) (pahoConnection, context.CancelFunc, error) {
		*operations = append(*operations, "dial")
		return dial(ctx)
	}
}

// startSteps keeps the history load, the broker session end and the dial.
func startSteps(operations []string) []string {
	return slices.DeleteFunc(slices.Clone(operations), func(op string) bool {
		return op != "list" && op != "end" && op != "dial"
	})
}

func TestStart_CarriesTheLegacyHistoryOverBeforeLoadingIt(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"legacy-identity": {"orders/#": {}},
	}}
	s := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	s.legacyManagedIdentity = "legacy-identity"

	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	assert.Equal(t, []string{"orders/#"}, store.snapshot("safe-session-id"))
	s.mu.Lock()
	_, loaded := s.managedHistory["orders/#"]
	s.mu.Unlock()
	assert.True(t, loaded, "the carried-over filter is cleaned up like any remembered one")
}

func TestStart_EndsTheBrokerSessionOfAnAddedKeyBeforeTheFirstConnection(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {},
	}}
	s := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	s.freshBrokerSessionPending = true
	startOrder(s, &operations)

	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	assert.Equal(t, []string{"list", "end", "dial"}, startSteps(operations))

	require.NoError(t, s.Reload(t.Context()))
	assert.Equal(t, []string{"list", "end", "dial", "dial"}, startSteps(operations),
		"a later connection resumes the broker session this session started")
}

func TestStart_LeavesTheBrokerSessionAloneOnceHistoryWasRecorded(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"orders/#": {}},
	}}
	s := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	s.freshBrokerSessionPending = true
	startOrder(s, &operations)

	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	assert.Equal(t, []string{"list", "dial"}, startSteps(operations),
		"another instance already connected as the identity and recorded what it subscribed")

	// The reconcile forgets every filter the plan no longer has.
	s.mu.Lock()
	clear(s.managedHistory)
	s.mu.Unlock()
	require.NoError(t, s.Reload(t.Context()))
	assert.Equal(t, []string{"list", "dial", "dial"}, startSteps(operations),
		"the broker session belongs to the history this session loaded")
}

func TestStart_FailsUntilTheBrokerSessionOfAnAddedKeyIsEnded(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {},
	}}
	s := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	s.freshBrokerSessionPending = true
	startOrder(s, &operations)
	endErr := errors.New("broker unreachable")
	s.endBrokerSessionOverride = func(context.Context) error {
		operations = append(operations, "end")
		return endErr
	}

	require.Error(t, s.Start(t.Context()))
	assert.Equal(t, []string{"list", "end"}, startSteps(operations), "no connection resumes the old broker session")

	endErr = nil
	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	assert.Equal(t, []string{"list", "end", "list", "end", "dial"}, startSteps(operations),
		"the retry reads the history again")
}

func TestStart_RetryLeavesTheBrokerSessionAloneOnceAnotherInstanceRecordedHistory(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {},
	}}
	s := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	s.freshBrokerSessionPending = true
	startOrder(s, &operations)
	s.endBrokerSessionOverride = func(context.Context) error {
		operations = append(operations, "end")
		return errors.New("broker unreachable")
	}

	require.Error(t, s.Start(t.Context()))
	require.NoError(t, store.Remember(t.Context(), "safe-session-id", []string{"orders/#"}))

	require.NoError(t, s.Start(t.Context()))
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	assert.Equal(t, []string{"list", "end", "list", "dial"}, startSteps(operations),
		"the other instance's broker session is what the recorded history describes")
}

func TestFactoryNewSession_CarriesTheLegacyIdentityAndTheAddedKey(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{}}
	config := &Config{Session: SessionOptions{BrokerURLs: []string{"tcp://192.0.2.1:1883"}, ClientID: "orders-client"}}

	raw, err := NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID:                                "orders-session",
		SessionMode:                       connectivity.SessionPersistent,
		Config:                            config,
		ManagedSubscriptionStore:          store,
		ManagedSubscriptionIdentity:       "safe-session-id",
		ManagedSubscriptionsRequired:      true,
		LegacyManagedSubscriptionIdentity: "legacy-identity",
		BrokerStateKeyAdded:               true,
	})
	require.NoError(t, err)
	s, ok := raw.(*Session)
	require.True(t, ok)
	assert.Equal(t, "legacy-identity", s.legacyManagedIdentity)
	assert.True(t, s.freshBrokerSessionPending)

	raw, err = NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID: "orders-session", SessionMode: connectivity.SessionEphemeral, Config: config, BrokerStateKeyAdded: true,
	})
	require.NoError(t, err)
	ephemeral, ok := raw.(*Session)
	require.True(t, ok)
	assert.False(t, ephemeral.freshBrokerSessionPending, "an ephemeral session always starts clean")
}
