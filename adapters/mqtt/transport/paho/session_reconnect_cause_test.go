package paho

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A rejected CONNECT is the ONE place MQTT reports why a session cannot come
// back: the broker authenticates at CONNECT only, and autopaho then retries
// forever behind the scenes. The mapped cause was reported to the reactive
// credential-refresh hook and otherwise discarded — Health latched only
// recovery/terminal errors — so readiness went red with nothing naming the
// reason. The cause is now latched until the session is up again, exposed on
// Health.LastError, and counted per bounded BridgeError code.

// TestReconnect_ConnectFailureCause_LatchedInHealthAndCounted pins the latch,
// the health exposure, and the bounded-code metric.
//
// Counterfactual (the pre-fix silence): handleConnectError mapped the error,
// reported auth failures, pushed a SessionReconnecting event and returned. The
// LastError assertion below sees nil and no metric exists.
func TestReconnect_ConnectFailureCause_LatchedInHealthAndCounted(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	logs := &recordingLogHandler{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reconnect-cause",
	}, connectivity.SessionPersistent, slog.New(logs), metrics)

	s.handleConnectError(errors.New("dial tcp 192.0.2.1:1883: connection refused"))

	health := s.Health(context.Background())
	require.Error(t, health.LastError,
		"readiness turning red must carry the reason the reconnect keeps failing")
	require.ErrorIs(t, health.LastError, shared.ErrUnavailable)
	require.False(t, health.Ready)

	entries := metrics.FindEntries(MetricMQTTConnectFailures)
	require.Len(t, entries, 1, "a rejected CONNECT is the reconnect-failure signal")
	require.Equal(t, "reconnect-cause", tagValue(entries[0].Tags, shared.TagKeySessionID))
	require.Equal(t, string(shared.ErrCodeUnavailable), tagValue(entries[0].Tags, shared.TagKeyCode),
		"the code dimension is the bounded BridgeError code, never a raw broker string")
	require.Equal(t, 1, logs.warnCountContaining("connect failed"))

	// The latch clears on the connection that actually comes up.
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.mu.Unlock()
	s.handleConnectionUp()
	require.NoError(t, s.Health(context.Background()).LastError,
		"a session that is back up must not keep reporting the failure that preceded it")
}

// TestReconnect_ConnectFailureCode_SeparatesAuthFromOutage proves the metric
// dimension is actionable: a revoked credential (CONNACK 0x87) and an
// unreachable broker must not land on the same series, because the operator
// response differs (rotate vs. wait).
func TestReconnect_ConnectFailureCode_SeparatesAuthFromOutage(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reconnect-cause-auth",
	}, connectivity.SessionPersistent, nil, metrics)

	s.handleConnectError(shared.ErrNotAuthorized.WithMessage("connack 0x87: not authorized"))

	entries := metrics.FindEntries(MetricMQTTConnectFailures)
	require.Len(t, entries, 1)
	require.Equal(t, string(shared.ErrCodeNotAuthorized), tagValue(entries[0].Tags, shared.TagKeyCode))
	require.ErrorIs(t, s.Health(context.Background()).LastError, shared.ErrNotAuthorized)
}
