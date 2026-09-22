package paho

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Helpers for the tests in this package that run against a real broker
// (testutil/mqttlocal). Those tests live in package paho because they observe
// session internals; the black-box suite in package paho_test keeps its own
// real-broker helpers (publishBacklog, watchTopic, requireDeliversAll), which
// an in-package test cannot import.

// brokerWait bounds every wait on a real broker: a delivery, a reconcile, a
// reconnect. A reconnect is the long one — when a broker restart leaves the old
// connection half-open, only the session's keep-alive notices.
const brokerWait = 30 * time.Second

// brokerMessage is what a receiver's handler saw of one delivery.
type brokerMessage struct {
	payload  string
	qos      int
	retained bool
}

// recordDeliveries runs a Receiver for topic on session and forwards every
// delivery, acknowledged, on the returned channel. The handler is registered
// before it returns, and the receiver stops when the test ends.
func recordDeliveries(t *testing.T, session *Session, receiverID, topic string) <-chan brokerMessage {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	delivered := make(chan brokerMessage, 16)
	receiver := NewReceiver(receiverID, session, WithTopicFilters(topic))
	stopped := make(chan error, 1)
	go func() {
		stopped <- receiver.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
			headers := delivery.Envelope().HeadersSnapshot()
			qos, _ := headers[HeaderMQTTQoS].(int)
			retained, _ := headers[HeaderMQTTRetained].(bool)
			select {
			case delivered <- brokerMessage{string(delivery.Envelope().Payload()), qos, retained}:
				return delivery.Ack(ctx)
			case <-ctx.Done():
				// The test is over and nothing reads the channel. A failed emit
				// would ask a persistent session to recycle its connection.
				return nil
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		// Canceled, not an emit error: an Ack that failed would have stopped the
		// receiver early, and every delivery after it would have gone unseen.
		require.ErrorIs(t, wait.RequireReceive(t, stopped, 5*time.Second), context.Canceled)
	})
	wait.RequireClosed(t, receiver.Started(), 5*time.Second)
	return delivered
}

// publishOnce publishes one message to topic at qos from a client of its own,
// the way any other client of the broker would. A retained message with an
// empty payload clears the retained message of topic.
func publishOnce(t *testing.T, brokerURL, topic string, qos byte, retain bool, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	publisher := NewSession(SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       mqttlocal.UniqueClientID("publisher"),
		KeepAlive:      10,
		ConnectTimeout: 10 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = publisher.Close(context.Background()) }()
	require.NoError(t, publisher.Start(ctx), "start the publisher")

	sender := NewSender(publisher, SenderOptions{QoS: qos, Retain: retain, Timeout: 10 * time.Second})
	require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: []byte(payload)}),
		Address:  topic,
	}), "publish %q to %s at QoS %d", payload, topic, qos)
}

// awaitReconciles waits until s has completed n reconciles, then until the last
// one has released the session's serialization gate. A runtime session manager
// over a real broker reconciles twice as it starts: once in Run, and once for
// the SessionConnected event the first connect queued. A test that drives the
// session's fake clock must let both finish before it advances: every reconcile
// replaces the QoS probe's timer, and a fake timer fires only inside Advance, so
// a timer replaced just after an Advance crossed its deadline waits for the next
// Advance and the probe the test awaits never runs. A reconcile of a plan with
// subscriptions emits MQTTReconcileLatency when it completes, and that is what
// is counted.
func awaitReconciles(t *testing.T, s *Session, metrics *ports.RecordingExporter, n int) {
	t.Helper()
	wait.Until(t, brokerWait, fmt.Sprintf("%d reconciles completed", n), func() bool {
		return len(metrics.FindEntries(MetricMQTTReconcileLatency)) >= n
	})
	awaitReloadGate(t, s, brokerWait, "await the last reconcile")
}
