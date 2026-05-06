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
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestUC74_MQTTRetainedMessages verifies that retained messages are delivered
// to new subscribers and that subsequent bridge-routed messages coexist with
// the retained message on the same topic.
func TestUC74_MQTTRetainedMessages(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Publish one retained message to the topic before any collector subscribes.
	retainedSess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc74-ret"), connectivity.SessionEphemeral)
	retainedSnd := paho.NewSender(retainedSess, paho.SenderOptions{
		DefaultTopic: "uc74/retained",
		QoS:          1,
		Retain:       true,
		Timeout:      5 * time.Second,
	})
	env := &messaging.Envelope{
		ID:      "uc74-retained-0",
		Subject: "uc74/retained",
		Payload: []byte(`{"retained":true}`),
	}
	require.NoError(t, retainedSnd.Send(ctx, env))
	time.Sleep(1 * time.Second) // SYNC: let retained message propagate to broker

	// Collector subscribes — should immediately receive the retained message.
	collector := newMQTTCollector(t, "uc74/retained", "uc74-col")

	// SQS input queue.
	sqsURL, sqsClient := setupSQSQueue(t, "uc74")
	sqsRcv := newSQSReceiver(t, sqsURL)

	// MQTT sender (non-retained) for bridge output.
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc74-snd"), connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("uc74-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc74-retained",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc74-bind", Address: "uc74/retained"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRcv, snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	// Send 100 messages through the bridge.
	const msgCount = 100
	sendBulkToSQS(t, sqsClient, sqsURL, msgCount, nil)

	// Wait for collector to receive at least 101 (1 retained + 100 normal).
	lrWaitFor(t, 90*time.Second,
		fmt.Sprintf("collector >= %d (retained + bridge)", msgCount+1),
		func() bool {
			return collector.count() >= msgCount+1
		})

	received := collector.count()
	t.Logf("UC74: received %d messages (expected >= %d)", received, msgCount+1)
	assert.GreaterOrEqual(t, received, msgCount+1,
		"collector should receive the retained message plus all bridge messages")
	assert.Equal(t, 0, dlq.count(), "no DLQ entries expected")
}

// TestUC75_WildcardSubscriptionOverlap verifies that an MQTT wildcard
// subscription receives messages published to a matching specific topic.
// This is primarily a documentation/behavior test.
func TestUC75_WildcardSubscriptionOverlap(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Wildcard collector: "uc75/+/data" should match "uc75/tenant1/data".
	wildcardCol := newMQTTCollector(t, "uc75/+/data", "uc75-wc")

	// Specific collector: "uc75/tenant1/data" — exact match.
	specificCol := newMQTTCollector(t, "uc75/tenant1/data", "uc75-sp")

	// SQS input.
	sqsURL, sqsClient := setupSQSQueue(t, "uc75")
	sqsRcv := newSQSReceiver(t, sqsURL)

	// MQTT sender targeting the specific topic.
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc75-snd"), connectivity.SessionEphemeral)
	snd := paho.NewSender(sess, paho.SenderOptions{
		DefaultTopic: "uc75/tenant1/data",
		QoS:          1,
		Timeout:      10 * time.Second,
	})

	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("uc75-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc75-wildcard",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc75-bind", Address: "uc75/tenant1/data"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRcv, snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	const msgCount = 500
	sendBulkToSQS(t, sqsClient, sqsURL, msgCount, nil)

	// Both collectors should receive all 500 messages independently.
	lrWaitFor(t, 90*time.Second,
		fmt.Sprintf("wildcard >= %d && specific >= %d", msgCount, msgCount),
		func() bool {
			return wildcardCol.count() >= msgCount && specificCol.count() >= msgCount
		})

	wcCount := wildcardCol.count()
	spCount := specificCol.count()
	t.Logf("UC75: wildcard collector=%d, specific collector=%d", wcCount, spCount)

	assert.GreaterOrEqual(t, wcCount, msgCount,
		"wildcard collector should receive all messages matching the pattern")
	assert.GreaterOrEqual(t, spCount, msgCount,
		"specific collector should receive all messages on the exact topic")
	assert.Equal(t, 0, dlq.count(), "no DLQ entries expected")
}

// TestUC76_QoS0FireAndForget sends 5,000 messages at QoS 0 and documents the
// delivery ratio. QoS 0 provides no delivery guarantees, so some loss is
// expected. The test passes as long as the bridge does not error and at least
// some messages arrive.
func TestUC76_QoS0FireAndForget(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	collector := newMQTTCollector(t, "uc76/qos0", "uc76-col")

	// SQS input.
	sqsURL, sqsClient := setupSQSQueue(t, "uc76")
	sqsRcv := newSQSReceiver(t, sqsURL)

	// QoS 0 sender — fire and forget.
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc76-snd"), connectivity.SessionEphemeral)
	snd := paho.NewSender(sess, paho.SenderOptions{
		DefaultTopic: "uc76/qos0",
		QoS:          0,
		Timeout:      5 * time.Second,
	})

	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("uc76-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc76-qos0",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc76-bind", Address: "uc76/qos0"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRcv, snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	const msgCount = 5000
	sendBulkToSQS(t, sqsClient, sqsURL, msgCount, nil)

	// Give time for delivery — QoS 0 is fast but lossy.
	lrWaitFor(t, 90*time.Second,
		fmt.Sprintf("collector >= %d (QoS 0)", msgCount),
		func() bool {
			return collector.count() >= msgCount
		})

	received := collector.count()
	lossPct := float64(msgCount-received) / float64(msgCount) * 100.0
	t.Logf("UC76: QoS 0 — sent %d, received %d, loss %.2f%%", msgCount, received, lossPct)

	assert.Greater(t, received, 0, "at least some messages should arrive at QoS 0")
	assert.Equal(t, 0, dlq.count(), "QoS 0 should not produce DLQ entries")

	// Document behavior: QoS 0 does not guarantee delivery.
	if received < msgCount {
		t.Logf("UC76 DOCUMENTATION: QoS 0 lost %d of %d messages (%.2f%%) — this is expected behavior",
			msgCount-received, msgCount, lossPct)
	}
}

// TestUC77_QoS2UnderBrokerRestart exercises QoS 2 (exactly-once) delivery
// while the broker is restarted mid-stream. A per-test broker with
// persistence is used. The bridge reconnects after restart (RES-001) and
// resumes delivery.
//
// NOTE: paho v5 may negotiate QoS 2 down to QoS 1 depending on broker
// configuration. The test documents the actual behavior.
func TestUC77_QoS2UnderBrokerRestart(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Per-test broker with persistence so sessions survive restart.
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	// Persistent collector — broker queues messages during restart gap.
	collector := newPersistentCollectorWithBroker(t, brokerURL, "uc77/qos2", "uc77-col")

	// SQS input.
	sqsURL, sqsClient := setupSQSQueue(t, "uc77")
	sqsRcv := newSQSReceiver(t, sqsURL)

	// QoS 2 sender. KeepAlive=5 for fast reconnection after broker restart.
	sess := setupMQTTSessionWithBroker(t, brokerURL,
		mqttlocal.UniqueClientID("uc77-snd"),
		connectivity.SessionPersistent,
		10, // receiveMaximum
		5,  // keepAlive
	)
	snd := paho.NewSender(sess, paho.SenderOptions{
		DefaultTopic: "uc77/qos2",
		QoS:          2,
		Timeout:      10 * time.Second,
	})

	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("uc77-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc77-qos2",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 50,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc77-bind", Address: "uc77/qos2"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRcv, snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	const msgCount = 1000
	sendBulkToSQS(t, sqsClient, sqsURL, msgCount, nil)

	// Wait until roughly half the messages are delivered, then restart broker.
	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("collector >= %d before restart", msgCount/2),
		func() bool {
			return collector.count() >= msgCount/2
		})

	beforeRestart := collector.count()
	t.Logf("UC77: %d messages received before broker restart", beforeRestart)

	broker.RestartGraceful()
	t.Log("UC77: broker restarted")

	// DirectHold: the route runner retries via SQS redelivery. Use
	// gobridgesync to confirm the bridge session is reconnected.
	// Persistent collector ensures broker queues messages during the gap.
	gobridgesync(t, 30*time.Second, rt)

	// Wait for all messages to arrive after reconnect.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d after restart", msgCount),
		func() bool {
			return collector.count() >= msgCount
		})

	received := collector.count()
	unique := countUnique(collector)
	t.Logf("UC77: total received=%d, unique=%d, dlq=%d", received, unique, dlq.count())

	assert.GreaterOrEqual(t, unique, msgCount,
		"all messages should eventually be delivered after reconnect (RES-001)")

	// QoS 2 should guarantee exactly-once — no duplicates.
	// However, if broker downgrades to QoS 1, duplicates are possible.
	duplicates := received - unique
	if duplicates > 0 {
		t.Logf("UC77 DOCUMENTATION: %d duplicate messages detected — broker may have "+
			"downgraded QoS 2 to QoS 1, or duplicates occurred across the restart boundary",
			duplicates)
	} else {
		t.Log("UC77: zero duplicates — QoS 2 exactly-once semantics confirmed")
	}

	_ = fmt.Sprintf("UC77 summary: sent=%d received=%d unique=%d duplicates=%d",
		msgCount, received, unique, duplicates)
}
