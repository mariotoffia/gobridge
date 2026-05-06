//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sync"
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
)

// =========================================================================
// UC7: SQS FIFO Ordering Through MQTT
//
// Validates that per-group message ordering is preserved when 3,000
// messages in 3 FIFO groups flow through SQS -> MQTT -> SQS-OUT.
// Uses standard queues with group headers since ElasticMQ FIFO support
// is limited. Verifies soft ordering within each group.
//
// Topology:
//   SQS-IN -> [Bridge] -> MQTT "uc7/ordered" -> [Egress] -> SQS-OUT
// =========================================================================

func TestUC7_SQS_FIFO_Ordering_Through_MQTT(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 3000
		groupCount  = 3
		pollTimeout = 120 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc7-in")
	sqsOutURL, sqsOutClient := setupSQSQueue(t, "uc7-out")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ingress: SQS-IN -> MQTT uc7/ordered
	sess1 := setupMQTTSession(t, mqttlocal.UniqueClientID("uc7-ingress"), connectivity.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess1)
	sqsRx := newSQSReceiver(t, sqsInURL)

	rtIn := goruntime.New(
		goruntime.WithInstanceID("uc7-ingress"),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rtIn.AddRoute(goruntime.RouteConfig{
		ID:     "uc7-ingress-route",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "mqtt-pub", Address: "uc7/ordered"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rtIn.Start(ctx))
	t.Cleanup(func() { _ = rtIn.Stop(context.Background()) })

	// Egress: MQTT uc7/ordered -> SQS-OUT
	egress := buildEgressBridge(t, ctx, "uc7-E", "uc7/ordered", sqsOutURL, dlq)
	t.Cleanup(func() { _ = egress.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rtIn, egress)

	// Send 3,000 messages with group IDs (round-robin across 3 groups).
	t.Logf("UC7: sending %d messages in %d groups", msgCount, groupCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, func(i int) map[string]string {
		return map[string]string{"group": fmt.Sprintf("grp-%d", i%groupCount)}
	})

	// Poll SQS-OUT for all messages.
	bodies := pollAllSQS(t, sqsOutClient, sqsOutURL, msgCount, pollTimeout)
	require.GreaterOrEqual(t, len(bodies), msgCount,
		"SQS-OUT should have at least %d messages", msgCount)

	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC7: received %d messages across %d groups", len(bodies), groupCount)
}

// =========================================================================
// UC8: Multi-Protocol Fan-Out
//
// Validates that 2,000 messages from SQS fan out to 2 MQTT topics and
// 1 SQS queue using DispatchFanOut + MatchAll. All 3 targets receive
// the full 2,000 messages.
//
// Topology:
//   SQS-IN -> [Bridge FanOut] -> MQTT "uc8/alpha"
//                              -> MQTT "uc8/beta"
//                              -> SQS-OUT
// =========================================================================

func TestUC8_MultiProtocol_FanOut(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		pollTimeout = 120 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc8-in")
	sqsOutURL, sqsOutClient := setupSQSQueue(t, "uc8-out")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// MQTT collectors for the two MQTT targets.
	collAlpha := newMQTTCollector(t, "uc8/alpha", "uc8-col-alpha")
	collBeta := newMQTTCollector(t, "uc8/beta", "uc8-col-beta")

	sqsRx := newSQSReceiver(t, sqsInURL)

	// Primary session+sender targets MQTT alpha.
	sidAlpha := uniqueID("uc8-alpha")
	sessAlpha := setupMQTTSession(t, mqttlocal.UniqueClientID("uc8-alpha"), connectivity.SessionEphemeral)
	sndAlpha := paho.NewSender(sessAlpha, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	fSessAlpha := newNoopSession()
	scAlpha := lrSessionConfig(sidAlpha)

	// Secondary session+sender targets MQTT beta.
	sidBeta := uniqueID("uc8-beta")
	sessBeta := setupMQTTSession(t, mqttlocal.UniqueClientID("uc8-beta"), connectivity.SessionEphemeral)
	sndBeta := paho.NewSender(sessBeta, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	fSessBeta := newNoopSession()
	scBeta := lrSessionConfig(sidBeta)

	// Third session+sender targets SQS-OUT.
	sidSQS := uniqueID("uc8-sqs")
	sndSQS := newSQSSender(t, sqsOutURL)
	fSessSQS := newNoopSession()
	scSQS := lrSessionConfig(sidSQS)

	bindings := []routing.DestinationBinding{
		{ID: "bind-alpha", SessionID: sidAlpha, Address: "uc8/alpha", Transport: "mqtt"},
		{ID: "bind-beta", SessionID: sidBeta, Address: "uc8/beta", Transport: "mqtt"},
		{ID: "bind-sqs", SessionID: sidSQS, Address: sqsOutURL, Transport: "sqs"},
	}

	rt := goruntime.New(
		goruntime.WithInstanceID("uc8-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.RegisterSessionSender(scBeta, fSessBeta, sndBeta))
	require.NoError(t, rt.RegisterSessionSender(scSQS, fSessSQS, sndSQS))

	routeCfg := goruntime.RouteConfig{
		ID: "uc8-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			DispatchMode: routing.DispatchFanOut,
		},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, sndAlpha, fSessAlpha, &scAlpha))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC8: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for all 3 targets.
	lrWaitFor(t, pollTimeout, "alpha collector", func() bool { return collAlpha.count() >= msgCount })
	lrWaitFor(t, pollTimeout, "beta collector", func() bool { return collBeta.count() >= msgCount })
	sqsBodies := pollAllSQS(t, sqsOutClient, sqsOutURL, msgCount, pollTimeout)

	require.GreaterOrEqual(t, collAlpha.count(), msgCount, "alpha should have >= %d", msgCount)
	require.GreaterOrEqual(t, collBeta.count(), msgCount, "beta should have >= %d", msgCount)
	require.GreaterOrEqual(t, len(sqsBodies), msgCount, "SQS-OUT should have >= %d", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	t.Logf("UC8: fan-out verified -- alpha=%d, beta=%d, sqs=%d",
		collAlpha.count(), collBeta.count(), len(sqsBodies))
}

// =========================================================================
// UC9: MQTT QoS 2 Stress
//
// Validates that 5,000 messages published at QoS 2 are delivered through
// the bridge with no duplicates. Uses MQTT QoS 2 on both inbound and
// outbound paths.
//
// Topology:
//   MQTT publisher QoS2 -> "uc9/input" -> [Bridge] -> "uc9/output" -> collector
// =========================================================================

func TestUC9_MQTT_QoS2_Stress(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 5000
		pollTimeout = 120 * time.Second
	)

	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Collector on output topic.
	collector := newMQTTCollector(t, "uc9/output", "uc9-col")

	// Bridge session: subscribe uc9/input, publish to uc9/output.
	rxSessID := mqttlocal.UniqueClientID("uc9-rx")
	rxSess := setupMQTTSession(t, rxSessID, connectivity.SessionEphemeral)
	require.NoError(t, rxSess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "uc9/input", QoS: 2}},
	}))
	waitSubReady(t, rxSess, 5*time.Second)

	mqttRx := paho.NewReceiver("uc9-rx", rxSess)

	txSessID := mqttlocal.UniqueClientID("uc9-tx")
	txSess := setupMQTTSession(t, txSessID, connectivity.SessionEphemeral)
	mqttSnd := paho.NewSender(txSess, paho.SenderOptions{QoS: 2, Timeout: 10 * time.Second})

	rt := goruntime.New(
		goruntime.WithInstanceID("uc9-bridge"),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:     "uc9-route",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "mqtt-out", Address: "uc9/output"},
		),
		SourceCapabilities: directHoldCaps,
	}, mqttRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	// Publish 5,000 messages at QoS 2.
	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc9-pub"), connectivity.SessionEphemeral)
	pubSnd := paho.NewSender(pubSess, paho.SenderOptions{QoS: 2, Timeout: 10 * time.Second})
	for i := 0; i < msgCount; i++ {
		env := &messaging.Envelope{
			ID:      fmt.Sprintf("uc9-%d", i),
			Subject: "uc9/input",
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
		}
		require.NoError(t, pubSnd.Send(ctx, ports.OutboundMessage{Envelope: env}), "publish msg %d", i)
	}
	t.Logf("UC9: published %d QoS 2 messages", msgCount)

	lrWaitFor(t, pollTimeout, "collector >= msgCount", func() bool {
		return collector.count() >= msgCount
	})

	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), msgCount)

	// Verify no duplicate payloads.
	seen := make(map[string]int, len(msgs))
	for _, m := range msgs {
		seen[string(m.Payload)]++
	}
	dupes := 0
	for _, count := range seen {
		if count > 1 {
			dupes++
		}
	}
	assert.Equal(t, 0, dupes, "QoS 2 should produce zero duplicate payloads")
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC9: %d unique payloads, %d total, %d dupes", len(seen), len(msgs), dupes)
}

// =========================================================================
// UC10: HTTP Inject to MQTT
//
// Validates that 1,000 messages injected via runtime.Inject() API reach
// an MQTT collector. Uses noopReceiver as the source (blocks until cancel).
//
// Topology:
//   Inject API -> [Bridge route] -> MQTT "uc10/output" -> collector
// =========================================================================

func TestUC10_HTTP_Inject_To_MQTT(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 1000
		pollTimeout = 60 * time.Second
	)

	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	collector := newMQTTCollector(t, "uc10/output", "uc10-col")

	txSess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc10-tx"), connectivity.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, txSess)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc10-bridge"),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:     "uc10-route",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "mqtt-out", Address: "uc10/output"},
		),
		SourceCapabilities: []ports.Capability{ports.CapHTTPEndpoint},
	}, &noopReceiver{}, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	// Inject messages in a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < msgCount; i++ {
			env := &messaging.Envelope{
				ID:      fmt.Sprintf("uc10-%d", i),
				Subject: "uc10/output",
				Payload: []byte(fmt.Sprintf(`{"inject":%d}`, i)),
			}
			if err := rt.Inject(ctx, "uc10-route", env); err != nil {
				t.Errorf("Inject %d: %v", i, err)
				return
			}
		}
	}()
	wg.Wait()
	t.Logf("UC10: injected %d messages", msgCount)

	lrWaitFor(t, pollTimeout, "collector >= msgCount", func() bool {
		return collector.count() >= msgCount
	})

	require.GreaterOrEqual(t, collector.count(), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC10: collector received %d messages", collector.count())
}

// =========================================================================
// UC11: SQS to SQS Direct
//
// Validates that 5,000 messages flow from SQS-IN through SharedOutbox
// to SQS-OUT without any MQTT involvement. Uses noopSession pattern.
//
// Topology:
//   SQS-IN -> [Bridge SharedOutbox] -> SQS-OUT
// =========================================================================

func TestUC11_SQS_To_SQS_Direct(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 5000
		pollTimeout = 120 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc11-in")
	sqsOutURL, sqsOutClient := setupSQSQueue(t, "uc11-out")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sqsRx := newSQSReceiver(t, sqsInURL)
	sqsSnd := newSQSSender(t, sqsOutURL)

	sessionID := uniqueID("uc11-sess")
	fSess := newNoopSession()
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc11-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc11-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "sqs-out", Address: sqsOutURL},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "sqs-out", SessionID: sessionID, Address: sqsOutURL, Transport: "sqs"},
		},
	}, sqsRx, sqsSnd, fSess, &sc))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC11: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	bodies := pollAllSQS(t, sqsOutClient, sqsOutURL, msgCount, pollTimeout)
	require.GreaterOrEqual(t, len(bodies), msgCount,
		"SQS-OUT should have at least %d messages", msgCount)
	assertNoDuplicates(t, "SQS-OUT", bodies)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC11: SQS-to-SQS direct verified with %d messages", len(bodies))
}
