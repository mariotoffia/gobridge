package paho

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

const ingressRejectTestClientID = "ingress-reject"

// newIngressRejectTestSession builds a persistent session that looks connected
// on a live connection, so a pre-decode reject lands on an up connection the
// way the guard sees it in production.
func newIngressRejectTestSession(t *testing.T) (*Session, *clocktest.Fake, *ports.RecordingExporter) {
	t.Helper()
	clk := clocktest.New()
	rec := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs:        []string{"tcp://192.0.2.1:1883"},
		ClientID:          ingressRejectTestClientID,
		Clock:             clk,
		ReconnectDelay:    time.Second,
		ReconnectMaxDelay: 8 * time.Second,
	}, connectivity.SessionPersistent, nil, rec)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.connUpAt = clk.Now().UnixNano()
	s.mu.Unlock()
	return s, clk, rec
}

func connectionGenerationOf(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectionGeneration
}

// replaceConnection drives the edges autopaho raises when the guard closes the
// socket and the redial succeeds: connection down, the reconnect delay, then
// connection up on the replacement.
func replaceConnection(t *testing.T, s *Session, clk *clocktest.Fake, reconnectDelay time.Duration) {
	t.Helper()
	gen := connectionGenerationOf(s)
	require.True(t, s.handleConnectionDownGeneration(gen), "the dropped connection must report down")
	clk.Advance(reconnectDelay)
	s.handleConnectionUpGeneration(gen)
}

func ingressRejectState(s *Session) (streak int, lastAt int64, rejectErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ingressRejectStreak, s.lastIngressRejectAt, s.ingressRejectErr
}

func TestSessionIngressReject_RejectDoesNotLatchSessionTerminal(t *testing.T) {
	s, _, rec := newIngressRejectTestSession(t)

	s.rejectPredecodeIngress(newMQTTMalformedError())

	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.NoError(t, terminalErr, "a pre-decode reject must not latch the session terminal")

	rejected := rec.FindEntries(MetricMQTTIngressRejected)
	require.Len(t, rejected, 1, "every pre-decode reject is counted once")
	assert.Equal(t, int64(1), rejected[0].IValue)
	assert.Contains(t, rejected[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: ingressRejectTestClientID})
	assert.Empty(t, rec.FindEntries(MetricMQTTRouterDropped),
		"a pre-decode reject is not router backpressure")

	assert.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)),
		"the connection the guard closed must still report down on the live generation")

	s.mu.Lock()
	eventsClosed := s.eventsClosed
	s.mu.Unlock()
	assert.False(t, eventsClosed, "the session keeps its lifecycle events open across a reject")
}

func TestSessionGuardIngress_MalformedPacketSendsDisconnectAndKeepsSessionAlive(t *testing.T) {
	s, _, rec := newIngressRejectTestSession(t)
	malformed := testPublishPacketWithPacketID(1, "guard/zero-packet-id", 0, nil, []byte("payload"))
	underlying := newTestNetConn(malformed, len(malformed))

	guarded, err := s.guardIngress(underlying)
	require.NoError(t, err)
	_, readErr := io.ReadAll(guarded)

	var ingressErr *mqttIngressError
	require.ErrorAs(t, readErr, &ingressErr)
	assert.Equal(t, mqttIngressMalformed, ingressErr.kind)
	assert.Equal(t, []byte{0xE0, 0x02, 0x81, 0x00}, underlying.Written(),
		"the broker is told the packet was malformed before the connection drops")
	assert.Equal(t, 1, underlying.CloseCount())

	s.mu.Lock()
	terminalErr := s.terminalErr
	s.mu.Unlock()
	assert.NoError(t, terminalErr, "a pre-decode reject must not latch the session terminal")
	assert.Len(t, rec.FindEntries(MetricMQTTIngressRejected), 1)
}

func TestSessionIngressReject_HealthNotReadyUntilReplacementConnectionStable(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)

	s.rejectPredecodeIngress(newMQTTMalformedError())

	health := s.Health(context.Background())
	assert.False(t, health.Ready, "a rejected connection is not ready")
	assert.Equal(t, ports.ServiceLevelNone, health.ServiceLevel)
	var ingressErr *mqttIngressError
	require.ErrorAs(t, health.LastError, &ingressErr, "LastError names the pre-decode reject")

	replaceConnection(t, s, clk, time.Second)
	health = s.Health(context.Background())
	assert.False(t, health.Ready, "a replacement connection is not ready until it has stayed up for the stability window")
	assert.Equal(t, ports.ServiceLevelNone, health.ServiceLevel)
	require.ErrorAs(t, health.LastError, &ingressErr)

	clk.Advance(connectionStabilityWindow)
	health = s.Health(context.Background())
	assert.True(t, health.Ready, "a replacement connection that stayed up for the stability window is ready")
	assert.NoError(t, health.LastError, "a settled reject no longer explains the session state")
}

func TestSessionIngressReject_ConnectionDownAfterStableWindowClearsReject(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	s.rejectPredecodeIngress(newMQTTMalformedError())
	replaceConnection(t, s, clk, time.Second)
	clk.Advance(connectionStabilityWindow)
	require.True(t, s.Health(context.Background()).Ready)

	require.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)))

	streak, lastAt, rejectErr := ingressRejectState(s)
	assert.NoError(t, rejectErr)
	assert.Zero(t, streak)
	assert.Zero(t, lastAt)
}

func TestSessionIngressReject_ConnectionDownBeforeStableWindowKeepsReject(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	cause := newMQTTMalformedError()
	s.rejectPredecodeIngress(cause)
	replaceConnection(t, s, clk, time.Second)
	clk.Advance(connectionStabilityWindow - time.Nanosecond)

	require.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)))

	streak, lastAt, rejectErr := ingressRejectState(s)
	assert.Same(t, cause, rejectErr, "a replacement that dropped before the window has not settled the reject")
	assert.Equal(t, 1, streak)
	assert.NotZero(t, lastAt)
	assert.False(t, s.Health(context.Background()).Ready)
}

func TestSessionIngressReject_StreakResetsAfterStableConnection(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	s.rejectPredecodeIngress(newMQTTMalformedError())
	s.rejectPredecodeIngress(newMQTTMalformedError())
	streak, _, _ := ingressRejectState(s)
	require.Equal(t, 2, streak, "rejects on a connection that never stabilised accumulate")

	replaceConnection(t, s, clk, time.Second)
	clk.Advance(connectionStabilityWindow)
	s.rejectPredecodeIngress(newMQTTMalformedError())

	streak, _, _ = ingressRejectState(s)
	assert.Equal(t, 1, streak, "a reject on a connection that stayed up for the stability window is a new incident")
}

func TestSessionIngressReject_ReconnectBackoffPenalisesRepeatedRejects(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	backoff := s.newReconnectBackoff(func() float64 { return 0 })

	require.Equal(t, time.Duration(0), backoff(0), "no reject: the redial after a connection that came up is immediate")

	s.rejectPredecodeIngress(newMQTTMalformedError())
	assert.Equal(t, 500*time.Millisecond, backoff(0), "first reject: the reconnect-delay floor")

	s.rejectPredecodeIngress(newMQTTMalformedError())
	assert.Equal(t, time.Second, backoff(0), "second reject: one backoff step")

	for i := 0; i < 10; i++ {
		s.rejectPredecodeIngress(newMQTTMalformedError())
	}
	assert.Equal(t, 4*time.Second, backoff(0), "a reject storm is capped at the reconnect max delay")

	clk.Advance(connectionStabilityWindow)
	assert.Equal(t, time.Duration(0), backoff(0), "no reject for the stability window: the penalty decays")
}

func TestSessionIngressReject_PlannedTeardownForgetsSettledReject(t *testing.T) {
	teardowns := []struct {
		name string
		// teardown ends the established connection the way the session does on
		// its own, without autopaho raising OnConnectionDown.
		teardown func(t *testing.T, s *Session)
		// reconnects reports whether the session can bring a new connection up
		// after the teardown.
		reconnects bool
	}{
		{
			name:       "disconnect generation",
			teardown:   func(_ *testing.T, s *Session) { s.disconnectGeneration(context.Background()) },
			reconnects: true,
		},
		{
			name:       "reload",
			teardown:   func(t *testing.T, s *Session) { require.NoError(t, s.Reload(context.Background())) },
			reconnects: true,
		},
		{
			name:     "close",
			teardown: func(t *testing.T, s *Session) { require.NoError(t, s.Close(context.Background())) },
		},
	}
	for _, tc := range teardowns {
		t.Run(tc.name, func(t *testing.T) {
			s, clk, _ := newIngressRejectTestSession(t)
			s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
				return &fakeLiveConn{}, func() {}, nil
			}
			s.rejectPredecodeIngress(newMQTTMalformedError())
			replaceConnection(t, s, clk, time.Second)
			clk.Advance(connectionStabilityWindow)
			require.True(t, s.Health(context.Background()).Ready)

			tc.teardown(t, s)

			streak, lastAt, rejectErr := ingressRejectState(s)
			assert.NoError(t, rejectErr, "a planned teardown of a connection that settled the reject forgets it")
			assert.Zero(t, streak)
			assert.Zero(t, lastAt)

			if !tc.reconnects {
				assert.NoError(t, s.Health(context.Background()).LastError,
					"a settled reject no longer explains the session state")
				return
			}
			require.NoError(t, s.Start(context.Background()))
			s.handleConnectionUp()
			health := s.Health(context.Background())
			assert.True(t, health.Ready, "the next connection is ready as soon as it is up")
			assert.NoError(t, health.LastError, "a settled reject no longer explains the session state")
		})
	}
}

func TestSessionIngressReject_RejectBeforeReplacementConnectionUpKeepsStreak(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	s.rejectPredecodeIngress(newMQTTMalformedError())
	require.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)))
	clk.Advance(connectionStabilityWindow)

	// Paho reads the replacement's first packet before autopaho raises its
	// connection-up edge, so connUpAt still names the dropped connection.
	s.rejectPredecodeIngress(newMQTTMalformedError())

	streak, _, _ := ingressRejectState(s)
	assert.Equal(t, 2, streak, "a reject before the replacement is up continues the storm, however long the reconnect delay")
}

func TestSessionIngressReject_ShortLivedConnectionDoesNotResetStreak(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	cause := newMQTTMalformedError()
	s.rejectPredecodeIngress(cause)
	replaceConnection(t, s, clk, time.Second)
	clk.Advance(5 * time.Second)
	require.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)))
	clk.Advance(connectionStabilityWindow)

	health := s.Health(context.Background())
	assert.False(t, health.Ready)
	assert.ErrorIs(t, health.LastError, cause, "a reject no connection has settled still explains the session state")

	s.rejectPredecodeIngress(newMQTTMalformedError())

	streak, _, _ := ingressRejectState(s)
	assert.Equal(t, 2, streak, "a connection that dropped before the stability window does not end the storm")
}

func TestSessionIngressReject_RejectOnLongLivedConnectionIsActiveUntilReplacementStable(t *testing.T) {
	s, clk, _ := newIngressRejectTestSession(t)
	clk.Advance(connectionStabilityWindow)
	cause := newMQTTMalformedError()

	s.rejectPredecodeIngress(cause)

	health := s.Health(context.Background())
	assert.False(t, health.Ready, "a reject on a long-lived connection is active at once")
	assert.ErrorIs(t, health.LastError, cause)

	require.True(t, s.handleConnectionDownGeneration(connectionGenerationOf(s)))
	streak, lastAt, rejectErr := ingressRejectState(s)
	assert.Same(t, cause, rejectErr, "the connection that carried the reject does not settle it")
	assert.Equal(t, 1, streak)
	assert.NotZero(t, lastAt)

	clk.Advance(time.Second)
	s.handleConnectionUpGeneration(connectionGenerationOf(s))
	health = s.Health(context.Background())
	assert.False(t, health.Ready, "a replacement is not ready until it has stayed up for the stability window")
	assert.ErrorIs(t, health.LastError, cause)

	clk.Advance(connectionStabilityWindow)
	health = s.Health(context.Background())
	assert.True(t, health.Ready, "a replacement that stayed up for the stability window settles the reject")
	assert.NoError(t, health.LastError)
}
