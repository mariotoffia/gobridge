package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestMQTTIngressPredecodeGuard_RealBrokerPropertyAmplificationNeverReachesSDKCallback(t *testing.T) {
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
		health := source.Health(ctx)
		var ingressErr *mqttIngressError
		return !health.Ready &&
			errors.As(health.LastError, &ingressErr) &&
			ingressErr.kind == mqttIngressUserPropertiesTooLarge
	}, 15*time.Second, 10*time.Millisecond)

	health := source.Health(ctx)
	assert.Zero(t, health.UnsettledCount,
		"predecode rejection must not enter Paho's acknowledgment tracker")
	dispatchDepth, _ := source.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Zero(t, source.router.PendingCount())
	assert.Zero(t, source.router.routeCount.Load(),
		"raw property amplification must not reach the Paho publish callback")
	assert.Zero(t, source.router.dropCount.Load(),
		"the decoded callback poison path must not observe a predecode rejection")

	terminalEvents := 0
	for {
		select {
		case event, ok := <-source.Events():
			if !ok {
				assert.Equal(t, 1, terminalEvents,
					"one raw violation must own one terminal lifecycle transition")
				return
			}
			if event.Type == ports.SessionError {
				terminalEvents++
				var ingressErr *mqttIngressError
				require.ErrorAs(t, event.Err, &ingressErr)
				assert.Equal(t, mqttIngressUserPropertiesTooLarge, ingressErr.kind)
			}
		case <-ctx.Done():
			t.Fatal("predecode terminal lifecycle did not close")
		}
	}
}

func TestIngressMemoryPoison_TerminalDisconnectRedeliversWithoutAckWedge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires a real local MQTT broker")
	}
	const topic = "ingress-memory/poison-redelivery"
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(2),
		mqttlocal.WithMessageSizeLimit(1024),
	)
	t.Cleanup(broker.Stop)

	clientID := mqttlocal.UniqueClientID("ingress-memory-poison")
	sourceOptions := SessionOptions{
		BrokerURLs:               []string{broker.URL()},
		ClientID:                 clientID,
		ConnectTimeout:           10 * time.Second,
		KeepAlive:                30,
		CleanStart:               false,
		SessionExpiryInterval:    60,
		ReceiveMaximum:           2,
		MaxPayloadBytes:          4,
		IngressMemoryBudgetBytes: DefaultIngressMemoryBudgetBytes,
	}
	source := NewSession(sourceOptions, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))

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

	send("12345")
	require.Eventually(t, func() bool {
		health := source.Health(ctx)
		return !health.Ready &&
			errors.Is(health.LastError, shared.ErrTransportClosedPermanently) &&
			health.UnsettledCount == 0
	}, 15*time.Second, 10*time.Millisecond,
		"oversize QoS 1 must enter one operator-visible terminal teardown without tracking an impossible ACK")
	require.NoError(t, source.Close(ctx))

	recoveredOptions := sourceOptions
	recoveredOptions.MaxPayloadBytes = 16
	recovered := NewSession(recoveredOptions, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = recovered.Close(context.Background()) })
	require.NoError(t, recovered.Start(ctx))
	require.NoError(t, recovered.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	receiver := NewReceiver("poison-redelivery", recovered, WithTopicFilters(topic))
	received := make(chan string, 2)
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
		t.Fatal("recovery receiver did not start")
	}

	select {
	case payload := <-received:
		require.Equal(t, "12345", payload, "terminally rejected packet must redeliver")
	case <-ctx.Done():
		t.Fatal("terminally rejected packet did not redeliver")
	}
	send("next")
	select {
	case payload := <-received:
		require.Equal(t, "next", payload, "redelivered poison ACK must not block later acknowledgements")
	case <-ctx.Done():
		t.Fatal("later QoS 1 delivery remained blocked after poison redelivery")
	}

	cancel()
	select {
	case err := <-receiverDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery receiver did not stop")
	}
}
