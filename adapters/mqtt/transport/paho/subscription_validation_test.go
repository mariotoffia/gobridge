package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A subscription reaches the broker only when the session manager reconciles
// the plan, long after the process accepted the configuration. A malformed
// filter therefore fails at activation instead of at build, and an
// out-of-range QoS is worse than a failure: the SDK masks it with & 0x03, so
// `qos: 4` silently becomes at-most-once delivery on a route that asked for
// at-least-once. Both are rejected where the configuration is first seen, and
// again in reconcile because a plan can be handed to a session directly.

func validationSession(t *testing.T, id string) *Session {
	t.Helper()
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   id,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { s.Router().shutdown() })
	return s
}

// TestFactoryNewReceiver_RejectsMalformedTopicFilter pins that every
// subscription filter is validated at the factory seam, not only the first
// non-empty one.
func TestFactoryNewReceiver_RejectsMalformedTopicFilter(t *testing.T) {
	filters := []string{
		"sensors/#/tail", // multi-level wildcard not in the final position
		"sensors/pre+/x", // single-level wildcard sharing a level
		"$share/g",       // shared filter without a nested filter
		"sensors/\x00/x", // null character
	}
	for _, filter := range filters {
		t.Run(filter, func(t *testing.T) {
			f := &Factory{}
			_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
				ID: "rx",
				Subscriptions: []connectivity.SubscriptionPlan{
					{Topic: "sensors/ok", QoS: 1},
					{Topic: filter, QoS: 1},
				},
			}, validationSession(t, "filter-"+filter))
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}
}

// TestFactoryNewReceiver_RejectsOutOfRangeQoS pins the QoS domain at the
// factory seam. 4 is the dangerous case: the SDK masks it to 0.
func TestFactoryNewReceiver_RejectsOutOfRangeQoS(t *testing.T) {
	for _, qos := range []int{-1, 3, 4} {
		f := &Factory{}
		_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
			ID:            "rx",
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/ok", QoS: qos}},
		}, validationSession(t, "qos"))
		require.ErrorIs(t, err, shared.ErrInvalidConfig, "qos %d must be rejected", qos)
	}
}

// TestFactoryNewReceiver_AcceptsLegalFiltersAndQoS guards against an
// over-broad rejection: wildcards, shared subscriptions and every legal QoS
// still build.
func TestFactoryNewReceiver_AcceptsLegalFiltersAndQoS(t *testing.T) {
	f := &Factory{}
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "rx",
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "sensors/+/temp", QoS: 0},
			{Topic: "sensors/#", QoS: 1},
			{Topic: "$share/group/sensors/#", QoS: 2},
		},
	}, validationSession(t, "legal"))
	require.NoError(t, err)
}

// TestReconcileSubscriptionConfig_RejectsOutOfRangeQoS pins the defensive check: a plan handed
// straight to a session (bypassing the factory) must not reach the broker with
// a QoS the SDK will silently mask.
func TestReconcileSubscriptionConfig_RejectsOutOfRangeQoS(t *testing.T) {
	conn := &ackAndErrorConn{}
	s, plan := ackSession(t, conn, connectivity.SubscriptionPlan{Topic: "sensors/x", QoS: 4})

	err := s.reconcile(context.Background(), conn, plan, nil, s.connEpoch)

	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}

// TestReconcileSubscriptionConfig_RejectsMalformedTopicFilter pins the same defensive check for
// the filter syntax.
func TestReconcileSubscriptionConfig_RejectsMalformedTopicFilter(t *testing.T) {
	conn := &ackAndErrorConn{}
	s, plan := ackSession(t, conn, connectivity.SubscriptionPlan{Topic: "sensors/#/tail", QoS: 1})

	err := s.reconcile(context.Background(), conn, plan, nil, s.connEpoch)

	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}

// TestFactoryNewReceiver_RejectsEmptyTopic pins that an empty filter is
// refused rather than skipped. The session plan is built from the same
// subscription list, so a topic this seam dropped would still be sent to the
// broker at reconcile — the two must agree.
func TestFactoryNewReceiver_RejectsEmptyTopic(t *testing.T) {
	f := &Factory{}
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "rx",
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "sensors/ok", QoS: 1},
			{Topic: "", QoS: 1},
		},
	}, validationSession(t, "empty-topic"))

	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}
