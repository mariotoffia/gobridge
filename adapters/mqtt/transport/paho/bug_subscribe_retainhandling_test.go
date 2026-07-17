package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// A-7 (MED): subscriptions must set MQTT5 RetainHandling per session mode. A
// persistent/exclusive session resumes across reconnects, so it re-SUBSCRIBEs
// with RetainHandling 1 ("send retained only if the subscription did not
// already exist"): the first subscribe still hydrates retained state, but every
// reconnect that resumes the session no longer triggers a full retained replay
// per filter (the storm the finding describes). An ephemeral session starts
// clean each connect and uses 0 (its subscription never pre-exists, so retained
// state must be sent every time).
//
// Mutation killed:
//   - flip retainHandlingForMode to return 0 for persistent → the persistent
//     sub_test's Equal(1, …) fails.
//   - flip it to return 1 for ephemeral → the ephemeral sub_test's Equal(0, …)
//     fails.
//   - drop `RetainHandling: retainHandlingForMode(s.mode)` at the build site →
//     both reconcile assertions see the zero value and the persistent case
//     fails.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_Subscribe_RetainHandling_ByMode(t *testing.T) {
	t.Run("mode mapping", func(t *testing.T) {
		require.Equal(t, byte(0), retainHandlingForMode(connectivity.SessionEphemeral),
			"ephemeral rehydrates retained state each connect")
		require.Equal(t, byte(1), retainHandlingForMode(connectivity.SessionPersistent),
			"persistent suppresses retained replay on resume")
		require.Equal(t, byte(1), retainHandlingForMode(connectivity.SessionExclusive),
			"exclusive resumes like persistent")
	})

	reconcileMode := func(t *testing.T, mode connectivity.SessionMode) *captureSubConn {
		t.Helper()
		sess := NewSession(SessionOptions{
			BrokerURLs:            []string{"tcp://192.0.2.1:1883"},
			ClientID:              "retain",
			SessionExpiryInterval: 300,
			Clock:                 testClock(),
		}, mode, nil)
		defer sess.Router().shutdown()

		fake := &captureSubConn{}
		sess.mu.Lock()
		sess.cm = fake
		sess.mu.Unlock()

		require.NoError(t, sess.Reconcile(context.Background(), connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/+/temp", QoS: 1}},
		}))
		return fake
	}

	t.Run("ephemeral subscribes with RetainHandling 0", func(t *testing.T) {
		spec, ok := reconcileMode(t, connectivity.SessionEphemeral).specFor("sensors/+/temp")
		require.True(t, ok, "the subscription was issued")
		require.Equal(t, byte(0), spec.RetainHandling,
			"A-7: an ephemeral session must rehydrate retained state on every connect")
	})

	t.Run("persistent subscribes with RetainHandling 1", func(t *testing.T) {
		spec, ok := reconcileMode(t, connectivity.SessionPersistent).specFor("sensors/+/temp")
		require.True(t, ok, "the subscription was issued")
		require.Equal(t, byte(1), spec.RetainHandling,
			"A-7: a persistent session must suppress the retained replay on reconnect")
	})
}
