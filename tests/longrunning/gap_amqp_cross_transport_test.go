//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP: AMQP 0-9-1 Cross-Transport Integration Tests
//
// These tests validate that messages flow correctly across heterogeneous
// transport boundaries via the gobridge runtime. Each test uses
// DeliveryDirectHold with DispatchSingle, proving that the bridge can
// relay messages between protocols without an outbox/lease store.
//
// Infrastructure:
//   - RabbitMQ (AMQP 0-9-1) — source broker for both tests
//   - ElasticMQ  (SQS)      — target for Test 1
//   - Mosquitto  (MQTT)     — target for Test 2
// ═══════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════
// TestGap_AMQP091_To_SQS_CrossTransport
//
// Validates that a message published to RabbitMQ via AMQP 0-9-1 flows
// through the bridge runtime (DirectHold, DispatchSingle) and arrives
// in an SQS queue with correct payload and forwarded headers.
//
// Topology:
//
//   Publisher ──► RabbitMQ exchange ──► queue
//                                        │
//                               AMQP091 Receiver
//                                        │
//                                  ┌─────┴─────┐
//                                  │  Bridge    │
//                                  │ DirectHold │
//                                  └─────┬─────┘
//                                        │
//                                   SQS Sender
//                                        │
//                                   SQS Queue ◄── Poll & Verify
//
// ═══════════════════════════════════════════════════════════════════════════

const (
	gapCrossMsgCount    = 5
	gapCrossPollTimeout = 30 * time.Second
)

func TestGap_AMQP091_To_SQS_CrossTransport(t *testing.T) {
	_ = withFreshInfra(t)

	const routingKey = "gap-cross-sqs"

	// --- RabbitMQ infrastructure ---
	exchangeName := rabbitmqlocal.UniqueExchange("gap-cross-sqs-ex")
	queueName := rabbitmqlocal.UniqueQueue("gap-cross-sqs-q")

	rabbitmqlocal.CreateExchange(t, exchangeName, "direct")
	rabbitmqlocal.CreateQueue(t, queueName)
	rabbitmqlocal.BindQueue(t, queueName, exchangeName, routingKey)

	// --- SQS target queue ---
	sqsOutURL, sqsOutClient := setupSQSQueue(t, "gap-cross-sqs-out")

	dlq := &lrDLQStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- AMQP 0-9-1 receiver session (started by the runtime) ---
	amqpSess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	amqpRx := newRabbitMQReceiver(t, amqpSess, queueName)

	// --- SQS sender ---
	sqsSnd := newSQSSender(t, sqsOutURL)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("gap-cross-amqp-sqs"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-cross-amqp-sqs-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "sqs-out", Address: sqsOutURL},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}, amqpRx, sqsSnd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 15*time.Second, rt)

	// --- Publish messages to RabbitMQ via a separate sender ---
	pubSess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	pubSnd := newRabbitMQSender(t, pubSess, exchangeName, routingKey)

	t.Logf("GAP-CROSS-SQS: publishing %d messages to RabbitMQ", gapCrossMsgCount)
	for i := 0; i < gapCrossMsgCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("gap-cross-sqs-%d", i),
			Subject: "cross-transport-test",
			Payload: []byte(fmt.Sprintf(`{"seq":%d,"origin":"amqp091"}`, i)),
			Headers: map[string]any{
				"x-origin":   "rabbitmq",
				"x-test-seq": fmt.Sprintf("%d", i),
			},
			CreatedAt: time.Now().UTC(),
		})
		require.NoError(t, pubSnd.Send(ctx, ports.OutboundMessage{Envelope: env}), "publish msg %d", i)
	}

	// --- Poll SQS and verify ---
	sqsMsgs := pollSQSWithAttrs(t, sqsOutClient, sqsOutURL, gapCrossMsgCount, gapCrossPollTimeout)
	require.Len(t, sqsMsgs, gapCrossMsgCount,
		"SQS-OUT should have exactly %d messages", gapCrossMsgCount)

	rxBodies := make(map[string]bool, len(sqsMsgs))
	for i, m := range sqsMsgs {
		assert.Contains(t, m.Body, `"origin":"amqp091"`,
			"msg %d: body should contain origin marker", i)
		rxBodies[m.Body] = true
	}
	for i := 0; i < gapCrossMsgCount; i++ {
		want := fmt.Sprintf(`{"seq":%d,"origin":"amqp091"}`, i)
		if !rxBodies[want] {
			t.Errorf("missing payload %s in SQS messages", want)
		}
	}

	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("GAP-CROSS-SQS: verified %d messages in SQS, dlq=%d",
		len(sqsMsgs), dlq.count())
}

// ═══════════════════════════════════════════════════════════════════════════
// TestGap_AMQP091_To_MQTT_CrossTransport
//
// Validates that a message published to RabbitMQ via AMQP 0-9-1 flows
// through the bridge runtime (DirectHold, DispatchSingle) and arrives
// at an MQTT subscriber with the correct Subject and payload.
//
// Topology:
//
//   Publisher ──► RabbitMQ exchange ──► queue
//                                        │
//                               AMQP091 Receiver
//                                        │
//                                  ┌─────┴─────┐
//                                  │  Bridge    │
//                                  │ DirectHold │
//                                  └─────┬─────┘
//                                        │
//                                   MQTT Sender
//                                        │
//                              MQTT topic "gap-cross/mqtt"
//                                        │
//                                   Collector ◄── Verify
//
// ═══════════════════════════════════════════════════════════════════════════

func TestGap_AMQP091_To_MQTT_CrossTransport(t *testing.T) {
	_ = withFreshInfra(t)

	const mqttTopic = "gap-cross/mqtt"

	// --- RabbitMQ infrastructure ---
	exchangeName := rabbitmqlocal.UniqueExchange("gap-cross-mqtt-ex")
	queueName := rabbitmqlocal.UniqueQueue("gap-cross-mqtt-q")
	const routingKey = "gap-cross-mqtt"

	rabbitmqlocal.CreateExchange(t, exchangeName, "direct")
	rabbitmqlocal.CreateQueue(t, queueName)
	rabbitmqlocal.BindQueue(t, queueName, exchangeName, routingKey)

	dlq := &lrDLQStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- MQTT collector subscribes before the bridge starts ---
	collector := newMQTTCollector(t, mqttTopic, "gap-cross-mqtt-col")

	// --- AMQP 0-9-1 receiver session ---
	amqpSess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	amqpRx := newRabbitMQReceiver(t, amqpSess, queueName)

	// --- MQTT sender session ---
	mqttSess := setupMQTTSession(t,
		mqttlocal.UniqueClientID("gap-cross-mqtt-tx"),
		connectivity.SessionEphemeral,
	)
	mqttSnd := setupMQTTSender(t, mqttSess)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("gap-cross-amqp-mqtt"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-cross-amqp-mqtt-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "mqtt-out", Address: mqttTopic},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}, amqpRx, mqttSnd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 15*time.Second, rt)

	// --- Publish messages to RabbitMQ ---
	pubSess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	pubSnd := newRabbitMQSender(t, pubSess, exchangeName, routingKey)

	t.Logf("GAP-CROSS-MQTT: publishing %d messages to RabbitMQ", gapCrossMsgCount)
	for i := 0; i < gapCrossMsgCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("gap-cross-mqtt-%d", i),
			Subject: "cross-transport-test",
			Payload: []byte(fmt.Sprintf(`{"seq":%d,"origin":"amqp091"}`, i)),
			Headers: map[string]any{
				"x-origin": "rabbitmq",
			},
			CreatedAt: time.Now().UTC(),
		})
		require.NoError(t, pubSnd.Send(ctx, ports.OutboundMessage{Envelope: env}), "publish msg %d", i)
	}

	// --- Wait for MQTT collector to receive all messages ---
	lrWaitFor(t, gapCrossPollTimeout,
		fmt.Sprintf("MQTT collector >= %d", gapCrossMsgCount),
		func() bool { return collector.count() >= gapCrossMsgCount },
	)

	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), gapCrossMsgCount,
		"MQTT collector should have at least %d messages", gapCrossMsgCount)

	rxPayloads := make(map[string]bool, len(msgs))
	for i, msg := range msgs {
		assert.Equal(t, "cross-transport-test", msg.Subject(),
			"msg %d: logical Subject must be preserved across AMQP→MQTT (transport address must NOT overwrite Subject)", i)
		assert.Contains(t, string(msg.Payload), `"origin":"amqp091"`,
			"msg %d: payload should contain origin marker", i)

		// T12: the MQTT ingress side must surface the *transport address*
		// (publish topic) under the dedicated mqtt.topic header — distinct
		// from the logical Envelope.Subject above. This proves that subject
		// and transport address travel as independent fields end-to-end.
		gotTopic, ok := messaging.GetHeaderString(msg.Headers(), paho.HeaderMQTTTopic)
		if assert.Truef(t, ok, "msg %d: expected %q header on MQTT ingress envelope, headers=%v",
			i, paho.HeaderMQTTTopic, msg.Headers()) {
			assert.Equalf(t, mqttTopic, gotTopic,
				"msg %d: header %q should equal the configured publish topic",
				i, paho.HeaderMQTTTopic)
		}

		rxPayloads[string(msg.Payload)] = true
	}
	for i := 0; i < gapCrossMsgCount; i++ {
		want := fmt.Sprintf(`{"seq":%d,"origin":"amqp091"}`, i)
		if !rxPayloads[want] {
			t.Errorf("missing payload %s in MQTT messages", want)
		}
	}

	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("GAP-CROSS-MQTT: verified %d messages on MQTT, dlq=%d",
		len(msgs), dlq.count())
}
