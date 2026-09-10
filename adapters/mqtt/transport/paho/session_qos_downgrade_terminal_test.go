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

// A broker that caps subscription QoS does not stop capping it. Every reconcile
// returns the same ErrQoSNotSupported, the runtime session manager treats it as
// a session failure, and the supervisor restarts the session forever at the
// backoff cap — an exclusive owner additionally releases and re-seizes its lease
// on every cycle, resetting each standby's observation window.
//
// A downgrade is therefore RETRYABLE only until the broker has confirmed the
// same grant enough times to prove it is policy rather than a blip. After that
// the error carries shared.ErrTransportClosedPermanently, which the session
// manager escalates to a terminal restart: readiness stays red, the fault is
// loud, and the churn stops.

// planAtQoS is the one-subscription plan these tests reconcile.
func planAtQoS(topic string, qos int) connectivity.SessionPlan {
	return connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: qos}},
	}
}

func newDowngradeSession(t *testing.T, clientID string, granted byte, logs slog.Handler) (*Session, *fakeReconcileConn) {
	t.Helper()
	fake := &fakeReconcileConn{reasons: []byte{granted}}
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(logs)
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   clientID,
	}, connectivity.SessionPersistent, logger, &ports.RecordingExporter{})
	s.mu.Lock()
	s.cm = fake
	s.connected = true
	empty := connectivity.SessionPlan{}
	s.appliedPlan = &empty
	s.mu.Unlock()
	return s, fake
}

// TestQoSDowngrade_RepeatedIdenticalGrant_BecomesPermanent pins the bounded
// confirmation: the same grant repeated permanentQoSDowngradeConfirmations
// times is a permanent incompatibility.
//
// Counterfactual (the pre-fix loop): every reconcile returned a bare
// ErrQoSNotSupported, so no confirmation count existed and the supervisor
// retried the identical failure forever — the ErrorIs on
// shared.ErrTransportClosedPermanently below never becomes true.
func TestQoSDowngrade_RepeatedIdenticalGrant_BecomesPermanent(t *testing.T) {
	logs := &recordingLogHandler{}
	s, _ := newDowngradeSession(t, "downgrade-permanent", 0x00, logs)
	plan := planAtQoS("sensors/x", 1)

	for attempt := 1; attempt < permanentQoSDowngradeConfirmations; attempt++ {
		err := s.Reconcile(context.Background(), plan)
		require.ErrorIs(t, err, shared.ErrQoSNotSupported)
		require.False(t, errors.Is(err, shared.ErrTransportClosedPermanently),
			"confirmation %d of %d must stay retryable — a single weak SUBACK can be a "+
				"transient broker state", attempt, permanentQoSDowngradeConfirmations)
	}

	err := s.Reconcile(context.Background(), plan)
	require.ErrorIs(t, err, shared.ErrQoSNotSupported,
		"the terminal error still classifies as the QoS incompatibility it is")
	require.ErrorIs(t, err, shared.ErrTransportClosedPermanently,
		"a confirmed permanent downgrade must escalate to a terminal restart instead of churning")

	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, "sensors/x", be.Context["topic"])
	require.Equal(t, permanentQoSDowngradeConfirmations, be.Context["confirmations"],
		"the operator needs to see how many identical grants proved this permanent")
	require.Equal(t, 1, logs.warnCountContaining("permanently incompatible"),
		"the escalation is announced exactly once, at the confirmation that proved it")
}

// TestQoSDowngrade_BrokerGrantsRequestedQoS_ResetsConfirmationStreak is the
// negative control. Lowering the route's requested QoS to what the broker
// allows is the operator's fix; it must clear the confirmation streak so a
// LATER, unrelated downgrade gets its full retry budget again rather than
// terminalising on the first occurrence.
func TestQoSDowngrade_BrokerGrantsRequestedQoS_ResetsConfirmationStreak(t *testing.T) {
	s, fake := newDowngradeSession(t, "downgrade-reset", 0x00, nil)

	// Two confirmations of "requested 1, granted 0".
	for i := 0; i < permanentQoSDowngradeConfirmations-1; i++ {
		require.ErrorIs(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 1)),
			shared.ErrQoSNotSupported)
	}

	// The operator lowers the route to QoS 0; the broker grants it, so this
	// reconcile converges and the streak is cleared.
	require.NoError(t, s.Reconcile(context.Background(), planAtQoS("sensors/x", 0)))
	require.Equal(t, 2, fake.subscribeCallCount(),
		"a changed requested QoS re-subscribes; an unchanged one does not")

	// Raising it again starts a fresh confirmation budget.
	for i := 0; i < permanentQoSDowngradeConfirmations-1; i++ {
		err := s.Reconcile(context.Background(), planAtQoS("sensors/x", 1))
		require.ErrorIs(t, err, shared.ErrQoSNotSupported)
		require.False(t, errors.Is(err, shared.ErrTransportClosedPermanently),
			"the successful reconcile must have reset the confirmation streak")
	}
}

// TestQoSDowngrade_DifferentTopic_DoesNotAccumulate proves the confirmation is
// per-grant, not a global failure counter: two different filters each
// downgraded once are two unrelated observations, not two confirmations of one
// permanent incompatibility.
func TestQoSDowngrade_DifferentTopic_DoesNotAccumulate(t *testing.T) {
	s, _ := newDowngradeSession(t, "downgrade-per-grant", 0x00, nil)

	for i := 0; i < permanentQoSDowngradeConfirmations+1; i++ {
		topic := "sensors/" + string(rune('a'+i))
		err := s.Reconcile(context.Background(), planAtQoS(topic, 1))
		require.ErrorIs(t, err, shared.ErrQoSNotSupported)
		require.False(t, errors.Is(err, shared.ErrTransportClosedPermanently),
			"a first downgrade on filter %q is one observation, not a confirmation", topic)
	}
}
