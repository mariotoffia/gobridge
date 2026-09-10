package paho

import (
	"context"
	"sync/atomic"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// An emit rejection on a DURABLE (QoS 1/2) delivery is recovered by withholding
// the acknowledgement and recycling the session so the broker redelivers it —
// but only a session that RESUMES has that contract. An ephemeral session dials
// clean_start=true on every connect, so its server-side state is discarded at
// the next disconnect and nothing is ever redelivered.
//
// Withholding the ack there buys no recovery and costs real capacity: the
// un-acked packet pins a broker Receive-Maximum slot for the life of the
// connection, and enough of them stop ingress entirely. The bounded ephemeral
// policy is therefore the same one QoS 0 already gets — ack, drop, and record
// the loss — so the receive window recovers.

// TestReceiverEmitRejection_EphemeralDurableDelivery_AcksAndCountsLost pins the
// bounded ephemeral rejection policy.
//
// Counterfactual (the pre-fix pin): the durable branch called requestRecovery,
// which returns ErrNotSupported for an ephemeral session and logged it at
// Debug. The delivery stayed un-acked forever, the slot stayed pinned, and the
// rejection was counted "recovering" — a recovery that could not happen.
func TestReceiverEmitRejection_EphemeralDurableDelivery_AcksAndCountsLost(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://127.0.0.1:1883"},
		ClientID:       "emit-rejected-ephemeral",
		ReceiveMaximum: 4,
	}, connectivity.SessionEphemeral, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	receiver := NewReceiver("rx", s, WithTopicFilters("orders/#"))
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(t.Context(), func(context.Context, ports.Delivery) error {
			return shared.ErrUnavailable.WithMessage("route runner is shutting down")
		})
	}()
	<-receiver.Started()

	// Wrap the ack exactly as the paho publish callback does, so the
	// Receive-Maximum window this test is about is the real one.
	var acked atomic.Int32
	ack := s.router.trackAcknowledgement(func() error { acked.Add(1); return nil })
	require.Equal(t, 1, s.Health(context.Background()).UnsettledCount,
		"precondition: the accepted QoS 1 packet holds a receive-window slot")

	s.router.dispatch(&pahov5.Publish{Topic: "orders/1", QoS: 1, PacketID: 7}, ack)
	require.Error(t, <-runDone)

	require.Equal(t, int32(1), acked.Load(),
		"an ephemeral session has no redelivery contract to protect, so the pinned "+
			"Receive-Maximum slot must be released")

	entries := metrics.FindEntries(MetricMQTTReceiverEmitRejected)
	require.Len(t, entries, 1)
	require.Equal(t, emitRejectionLost, tagValue(entries[0].Tags, shared.TagKeyOutcome),
		"nothing redelivers it, so the outcome is loss — never a recovery that cannot happen")

	require.Zero(t, s.Health(context.Background()).UnsettledCount,
		"the receive window must not stay pinned by a delivery no one will redeliver")
}

// TestReceiverEmitRejection_EphemeralRejection_DoesNotRecycleTheSession is the
// complementary guard: a recycle cannot recover an ephemeral delivery, so the
// rejection must not request one (a recycle would only drop every OTHER
// in-flight delivery on the session).
func TestReceiverEmitRejection_EphemeralRejection_DoesNotRecycleTheSession(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "emit-rejected-ephemeral-norecycle",
	}, connectivity.SessionEphemeral, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	receiver := NewReceiver("rx", s, WithTopicFilters("orders/#"))
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(t.Context(), func(context.Context, ports.Delivery) error {
			return shared.ErrUnavailable.WithMessage("route runner is wedged")
		})
	}()
	<-receiver.Started()

	s.router.dispatch(&pahov5.Publish{Topic: "orders/2", QoS: 2, PacketID: 8}, func() error { return nil })
	require.Error(t, <-runDone)

	s.mu.Lock()
	pending := s.recoveryPending
	s.mu.Unlock()
	require.False(t, pending, "no settlement recovery may be queued for an ephemeral session")
	require.Empty(t, metrics.FindEntries(MetricMQTTSessionRecoveryRecycle))
}
