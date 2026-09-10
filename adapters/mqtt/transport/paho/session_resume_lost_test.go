package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Persistent and exclusive sessions dial clean_start=false because they want
// the broker to RESUME: the subscriptions and the queued offline QoS 1/2
// backlog survive the disconnect. CONNACK answers that request with Session
// Present. When it comes back FALSE the broker has nothing to resume — the
// session expired during a long outage, the broker restarted without
// persistence, or an exclusive standby connected after session_expiry_interval
// elapsed — and the offline backlog is gone.
//
// Re-subscribing then succeeds and the session reports itself healthy, so the
// discontinuity is invisible: an operator relying on persistent/exclusive
// continuity cannot tell that a failover dropped the backlog. The loss is now
// counted, warned, and latched on Health.LastError until the next successful
// reconcile has re-established the subscriptions.

func newResumeSession(t *testing.T, mode connectivity.SessionMode, clientID string, logs slog.Handler) (*Session, *ports.RecordingExporter) {
	t.Helper()
	metrics := &ports.RecordingExporter{}
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(logs)
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   clientID,
	}, mode, logger, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.mu.Unlock()
	return s, metrics
}

// TestReconnect_PersistentResumeWithoutSessionPresent_SignalsLostBacklog pins
// the metric, the warning and the Health latch on the reconnect edge.
//
// Counterfactual (the pre-fix silence): sessionPresent was consulted only for
// the settlement-recovery path, so an ordinary reconnect reset state, reported
// connected and re-subscribed. Every assertion below fails.
func TestReconnect_PersistentResumeWithoutSessionPresent_SignalsLostBacklog(t *testing.T) {
	logs := &recordingLogHandler{}
	s, metrics := newResumeSession(t, connectivity.SessionPersistent, "resume-lost", logs)

	// First connect: nothing to resume yet, so no loss can be claimed.
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)
	require.Empty(t, metrics.FindEntries(MetricMQTTSessionResumeLost),
		"a cold start has no durable session to lose")

	// The reconnect asked the broker to resume and it could not.
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)

	entries := metrics.FindEntries(MetricMQTTSessionResumeLost)
	require.Len(t, entries, 1)
	require.Equal(t, "resume-lost", tagValue(entries[0].Tags, shared.TagKeySessionID))
	require.Equal(t, 1, logs.warnCountContaining("broker did not resume the durable session"))

	health := s.Health(context.Background())
	require.ErrorIs(t, health.LastError, shared.ErrNotFound,
		"the offline QoS 1/2 backlog queued for this client id is gone")
	require.True(t, health.Connected, "the session IS connected; only its continuity was broken")
}

// TestReconnect_ExclusiveStandbyWithDurableHistory_SignalsLostBacklog covers
// the failover shape the cold-start exemption would otherwise hide: a standby's
// FIRST connect in its own process is still a resume attempt, and its managed
// subscription history is durable proof that this client id previously held
// broker-side filters.
func TestReconnect_ExclusiveStandbyWithDurableHistory_SignalsLostBacklog(t *testing.T) {
	s, metrics := newResumeSession(t, connectivity.SessionExclusive, "resume-lost-standby", nil)
	s.mu.Lock()
	s.managedRequired = true
	s.managedLoaded = true
	s.managedHistory = map[string]struct{}{"sensors/#": {}}
	s.mu.Unlock()

	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)

	require.Len(t, metrics.FindEntries(MetricMQTTSessionResumeLost), 1,
		"durable history proves the broker held filters for this identity before this connect")
}

// TestReconnect_PersistentResumeWithSessionPresent_IsNotALoss and the ephemeral
// case are the negative controls: neither is a discontinuity, so neither may
// raise the signal an operator would page on.
func TestReconnect_PersistentResumeWithSessionPresent_IsNotALoss(t *testing.T) {
	s, metrics := newResumeSession(t, connectivity.SessionPersistent, "resume-ok", nil)
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, true)
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, true)
	require.Empty(t, metrics.FindEntries(MetricMQTTSessionResumeLost))
	require.NoError(t, s.Health(context.Background()).LastError)
}

func TestReconnect_EphemeralWithoutSessionPresent_IsNotALoss(t *testing.T) {
	s, metrics := newResumeSession(t, connectivity.SessionEphemeral, "resume-ephemeral", nil)
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)
	require.Empty(t, metrics.FindEntries(MetricMQTTSessionResumeLost),
		"an ephemeral session dials clean_start=true and expects no resumption")
	require.NoError(t, s.Health(context.Background()).LastError)
}

// TestReconnect_ResumeLossLatch_ClearedByNextReconcile pins the latch lifetime:
// it stays until the subscriptions are re-established, then clears, so it names
// the loss window without wedging health forever.
func TestReconnect_ResumeLossLatch_ClearedByNextReconcile(t *testing.T) {
	s, _ := newResumeSession(t, connectivity.SessionPersistent, "resume-latch", nil)
	fake := &fakeReconcileConn{}
	s.mu.Lock()
	s.cm = fake
	empty := connectivity.SessionPlan{}
	s.appliedPlan = &empty
	s.mu.Unlock()

	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)
	s.handleConnectionUpGenerationWithSessionPresent(s.connectionGeneration, false)
	require.ErrorIs(t, s.Health(context.Background()).LastError, shared.ErrNotFound)

	require.NoError(t, s.Reconcile(context.Background(),
		connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/x", QoS: 1}}}))
	require.NoError(t, s.Health(context.Background()).LastError,
		"the subscriptions are back, so the loss window is closed")
}
