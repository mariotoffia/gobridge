package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

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
		s.EndBrokerStateOnClose()

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
	s.EndBrokerStateOnClose()

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
	for name, cm := range map[string]pahoConnection{
		"never connected": nil,
		// autopaho lost the connection and is reconnecting as the client ID.
		"reconnecting": &fakeLiveConn{},
	} {
		t.Run(name, func(t *testing.T) {
			var ends atomic.Int32
			s := NewSession(SessionOptions{ClientID: "orders-client"}, connectivity.SessionPersistent, nil)
			s.endBrokerSessionOverride = func(context.Context) error {
				ends.Add(1)
				return nil
			}
			s.mu.Lock()
			s.cm = cm
			s.connected = false
			s.mu.Unlock()
			s.EndBrokerStateOnClose()

			require.NoError(t, s.Close(t.Context()))

			assert.Zero(t, ends.Load(), "only an instance connected as the identity ends its state")
		})
	}
}

func TestClose_EndsNothingForAnEphemeralSession(t *testing.T) {
	var ends atomic.Int32
	s := connectedSessionCountingEnds(connectivity.SessionEphemeral, &ends, nil)
	s.EndBrokerStateOnClose()

	require.NoError(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load())
}

func TestClose_CountsAFailureToEndTheBrokerSessionAndStillCloses(t *testing.T) {
	var ends atomic.Int32
	metrics := &ports.RecordingExporter{}
	s := connectedSessionCountingEnds(connectivity.SessionPersistent, &ends, errors.New("broker unreachable"), metrics)
	s.sessionID = "orders-session"
	s.EndBrokerStateOnClose()

	require.NoError(t, s.Close(t.Context()), "a failure to end broker state never fails the reload")

	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1)
	assert.Equal(t, int64(1), entries[0].IValue)
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
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
	s.EndBrokerStateOnClose()

	require.Error(t, s.Close(t.Context()))

	assert.Zero(t, ends.Load(), "autopaho may still be connected or reconnecting as the client ID")
	entries := metrics.FindEntries(shared.MetricBrokerStateEndFailures)
	require.Len(t, entries, 1, "the broker session was not ended")
	assert.Contains(t, entries[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "orders-session"})
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
