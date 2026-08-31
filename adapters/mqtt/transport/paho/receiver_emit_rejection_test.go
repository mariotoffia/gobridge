package paho

import (
	"context"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// An emit rejection — the route runner refusing a delivery during shutdown, or
// a wedged pipeline — has two very different outcomes by QoS, and both must be
// counted:
//
//   - QoS 1/2 (durable): the un-acked delivery is recovered by a bounded session
//     recycle, so the broker redelivers it. Observable through the recycle, but
//     the rejection RATE is the leading signal.
//   - QoS 0: there is no acknowledgement to withhold and no redelivery contract.
//     The message is simply gone. Nothing recorded it.

// TestReceiverEmitRejection_QoS0LossIsCounted pins the QoS 0 half: the rejected
// message is unrecoverable, so it must be visible as a loss.
//
// Counterfactual (the pre-fix silence): the QoS 0 branch logged at Debug and
// returned. With debug logging off — the production default — a shutdown or a
// wedged route discarded messages leaving no counter, no metric, and no trace.
func TestReceiverEmitRejection_QoS0LossIsCounted(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "emit-rejected-qos0",
	}, connectivity.SessionPersistent, nil, metrics)

	receiver := NewReceiver("rx", s, WithTopicFilters("orders/#"))
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(t.Context(), func(context.Context, ports.Delivery) error {
			return shared.ErrUnavailable.WithMessage("route runner is shutting down")
		})
	}()
	<-receiver.Started()

	// QoS 0 carries no protocol acknowledgement, so the router hands the
	// receiver a nil ack: nothing can be withheld and nothing is redelivered.
	s.router.dispatch(&pahov5.Publish{Topic: "orders/1", QoS: 0}, nil)
	require.Error(t, <-runDone)

	entries := metrics.FindEntries(MetricMQTTReceiverEmitRejected)
	require.Len(t, entries, 1, "a QoS 0 emit rejection is an unrecoverable loss and must be counted")
	require.Equal(t, tagValue(entries[0].Tags, shared.TagKeyOutcome), emitRejectionLost,
		"the outcome tag must separate the unrecoverable loss from a recoverable rejection")
	require.Equal(t, tagValue(entries[0].Tags, shared.TagKeySessionID), "emit-rejected-qos0")
}

// TestReceiverEmitRejection_DurableRejectionIsCountedAsRecovering pins the
// QoS 1/2 half: the same counter fires, tagged as recoverable, because the
// bounded session recycle redelivers the un-acked message.
func TestReceiverEmitRejection_DurableRejectionIsCountedAsRecovering(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "emit-rejected-qos1",
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()
	// Park the recycle so the test observes only the rejection accounting.
	s.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	receiver := NewReceiver("rx", s, WithTopicFilters("orders/#"))
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(t.Context(), func(context.Context, ports.Delivery) error {
			return shared.ErrUnavailable.WithMessage("route runner is shutting down")
		})
	}()
	<-receiver.Started()

	s.router.dispatch(&pahov5.Publish{Topic: "orders/1", QoS: 1}, func() error { return nil })
	require.Error(t, <-runDone)

	entries := metrics.FindEntries(MetricMQTTReceiverEmitRejected)
	require.Len(t, entries, 1)
	require.Equal(t, tagValue(entries[0].Tags, shared.TagKeyOutcome), emitRejectionRecovering)
}

func tagValue(tags []shared.Tag, key string) string {
	for _, tag := range tags {
		if tag.Key == key {
			return tag.Value
		}
	}
	return ""
}
