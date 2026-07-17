package paho

import (
	"context"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestMQTTIngressPoison_RealBrokerPropertyAmplificationAckDroppedWithoutTerminal
// pins the MQTT-L1 escape against a real broker: a publish exceeding the User
// Property count cap fits the advertised Maximum Packet Size, so the broker
// forwards it. The adapter must ACK-and-DROP it (freeing the broker in-flight
// slot, counting MQTTIngressPoisonDropped) and the session must stay healthy
// — the pre-fix behavior latched the session terminal, handing any authorized
// publisher a permanent kill switch via broker redelivery on every resume.
func TestMQTTIngressPoison_RealBrokerPropertyAmplificationAckDroppedWithoutTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const topic = "ingress-memory/predecode-property-amplification"
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(4),
		mqttlocal.WithMessageSizeLimit(1<<20),
	)
	t.Cleanup(broker.Stop)

	source := NewSession(SessionOptions{
		BrokerURLs:               []string{broker.URL()},
		ClientID:                 mqttlocal.UniqueClientID("ingress-predecode-source"),
		ConnectTimeout:           10 * time.Second,
		KeepAlive:                30,
		CleanStart:               true,
		ReceiveMaximum:           4,
		MaxPayloadBytes:          16,
		IngressMemoryBudgetBytes: DefaultIngressMemoryBudgetBytes,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))

	publisher := NewSession(SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("ingress-predecode-publisher"),
		ConnectTimeout: 10 * time.Second,
		KeepAlive:      30,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx))

	properties := make(pahov5.UserProperties, maxIngressUserProperties+1)
	for i := range properties {
		properties[i] = pahov5.UserProperty{Key: "", Value: ""}
	}
	_, err := publisher.ConnectionManager().Publish(ctx, &pahov5.Publish{
		Topic:      topic,
		QoS:        1,
		Payload:    []byte("ok"),
		Properties: &pahov5.PublishProperties{User: properties},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return source.router.IngressPoisonDroppedCount() == 1
	}, 15*time.Second, 10*time.Millisecond,
		"over-cap user properties must be acked-and-dropped on the poison counter")

	health := source.Health(ctx)
	assert.True(t, health.Ready, "a poison publish must NOT latch the session terminal (MQTT-L1)")
	assert.NoError(t, health.LastError)
	assert.Zero(t, health.UnsettledCount,
		"the poison drop settles immediately; it must not linger in the unsettled window")
	dispatchDepth, _ := source.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Zero(t, source.router.PendingCount())
	assert.Zero(t, source.router.routeCount.Load(),
		"a poison publish must be dropped before handler dispatch")
}

// TestMQTTIngressPoison_OversizePayloadAckFreesSlotAndLaterTrafficFlows pins
// the full MQTT-L1 escape sequence for the easiest poison to trigger — a
// payload one byte over max_payload_bytes, well inside the advertised
// Maximum Packet Size: the packet is acked-and-dropped, the session stays
// up, a later good publish flows normally, and after a Reload (reconnect,
// clean_start=false persistent session) the broker does NOT redeliver the
// poison — proving the ack genuinely freed the broker-side slot rather than
// parking the packet for the next resume (the pre-fix terminal loop).
func TestMQTTIngressPoison_OversizePayloadAckFreesSlotAndLaterTrafficFlows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const topic = "ingress-memory/poison-ack-drop"
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(2),
		mqttlocal.WithMessageSizeLimit(1024),
	)
	t.Cleanup(broker.Stop)

	clientID := mqttlocal.UniqueClientID("ingress-memory-poison")
	source := NewSession(SessionOptions{
		BrokerURLs:               []string{broker.URL()},
		ClientID:                 clientID,
		ConnectTimeout:           10 * time.Second,
		KeepAlive:                30,
		CleanStart:               false,
		SessionExpiryInterval:    60,
		ReceiveMaximum:           2,
		MaxPayloadBytes:          4,
		IngressMemoryBudgetBytes: DefaultIngressMemoryBudgetBytes,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))

	receiver := NewReceiver("poison-ack-drop", source, WithTopicFilters(topic))
	received := make(chan string, 4)
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(ctx, func(emitCtx context.Context, delivery ports.Delivery) error {
			payload := string(delivery.Envelope().Payload())
			if err := delivery.Ack(emitCtx); err != nil {
				return err
			}
			received <- payload
			return nil
		})
	}()
	select {
	case <-receiver.Started():
	case <-ctx.Done():
		t.Fatal("receiver did not start")
	}

	publisher := NewSession(SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("ingress-memory-poison-publisher"),
		ConnectTimeout: 10 * time.Second,
		KeepAlive:      30,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx))
	sender := NewSender(publisher, SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	send := func(payload string) {
		t.Helper()
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
			Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(payload)}),
			Address:  topic,
		}))
	}

	send("12345") // 5 bytes > max_payload_bytes 4, within broker + advertised limits
	require.Eventually(t, func() bool {
		return source.router.IngressPoisonDroppedCount() == 1
	}, 15*time.Second, 10*time.Millisecond,
		"oversize payload must be acked-and-dropped on the poison counter")

	health := source.Health(ctx)
	require.True(t, health.Ready, "a poison publish must NOT latch the session terminal (MQTT-L1)")

	send("good") // 4 bytes: within the cap; the same connection must still deliver
	select {
	case payload := <-received:
		require.Equal(t, "good", payload, "traffic after a poison drop must flow on the same connection")
	case <-ctx.Done():
		t.Fatal("later QoS 1 delivery blocked after poison ack-drop")
	}

	// Reconnect the persistent session: the acked poison must NOT be
	// redelivered (the ack freed the broker slot). A resumed session replays
	// un-acked backlog before/alongside new traffic, so observing the
	// post-reload marker with no "12345" in the channel proves no replay.
	require.NoError(t, source.Reload(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	send("post")
	select {
	case payload := <-received:
		require.Equal(t, "post", payload,
			"resumed session must deliver only new traffic — an acked poison packet must not be redelivered")
	case <-ctx.Done():
		t.Fatal("post-reload delivery did not arrive")
	}
	assert.Equal(t, int64(1), source.router.IngressPoisonDroppedCount(),
		"the poison packet must not be redelivered (and re-dropped) after reconnect")

	cancel()
	select {
	case err := <-receiverDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not stop")
	}
}
