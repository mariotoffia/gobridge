package paho

import (
	"context"
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

func TestSessionIngressReject_RejectDropsConnectionWithoutTerminalLatch(t *testing.T) {
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
