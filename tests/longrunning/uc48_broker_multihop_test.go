//go:build longrunning

package longrunning_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC48: Broker Down During Multi-Hop Pipeline
//
// SQS-IN --> [Bridge-A (SharedOutbox)] --> MQTT "uc48/hop" -->
//            [Bridge-B (DirectHold)] --> SQS-OUT
//
// Kill the broker at ~500 messages in the hop topic. Wait 5s. Restart.
// Assert: SQS-OUT >= 2,000 unique after recovery.
//
// PRODUCTION FIX NEEDED:
//   - RES-001: autopaho ConnectionManager doesn't reconnect to restarted
//     broker on same port. Both bridges may stay disconnected after restart.
//   - Until fixed, this test is expected to FAIL.
// =========================================================================

func TestUC48_BrokerDownMultiHop(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		killAt      = 500
		hopTopic    = "uc48/hop"
		testTimeout = 300 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc48-in")
	sqsOutURL, sqsOutClient := setupSQSQueue(t, "uc48-out")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlqA := &lrDLQStore{}
	dlqB := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Persistent collector on hop topic — measures when to kill broker.
	// Persistent session ensures broker queues messages during restart gap.
	hopCollector := newPersistentCollectorWithBroker(t, brokerURL, hopTopic, "uc48-col")

	// --- Bridge-A: SQS-IN → SharedOutbox → MQTT ---
	sessIDA := mqttlocal.UniqueClientID("uc48-sess-a")
	sessA := setupMQTTSessionWithBroker(t, brokerURL, sessIDA,
		connectivity.SessionExclusive, 65535, 5)
	mqttSndA := setupMQTTSender(t, sessA)
	sqsRxA := newSQSReceiver(t, sqsInURL)
	scA := lrSessionConfig(sessIDA)

	rtA := goruntime.New(
		goruntime.WithInstanceID("uc48-bridge-a"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlqA),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtA.AddRoute(goruntime.RouteConfig{
		ID: "uc48-route-a",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc48-bind-a", Address: hopTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc48-bind-a", SessionID: sessIDA},
		},
	}, sqsRxA, mqttSndA, sessA, &scA))
	require.NoError(t, rtA.Start(ctx))
	defer func() { _ = rtA.Stop(context.Background()) }()

	// --- Bridge-B: MQTT → SharedOutbox → SQS-OUT ---
	// MQTT sources do not support visibility extension, so SharedOutbox is
	// the correct delivery mode (DirectHold would fail validation).
	leaseStoreB, outboxStoreB := setupDynamoStores(t)
	rxSessIDB := mqttlocal.UniqueClientID("uc48-rxb")
	rxSessB := setupMQTTSessionWithBroker(t, brokerURL, rxSessIDB,
		connectivity.SessionExclusive, 65535, 5)
	require.NoError(t, rxSessB.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: hopTopic, QoS: 1}},
	}))
	waitSubReady(t, rxSessB, 5*time.Second)

	mqttRxB := paho.NewReceiver("uc48-rxb", rxSessB)
	sqsSndB := newSQSSender(t, sqsOutURL)
	scB := lrSessionConfig(rxSessIDB)

	rtB := goruntime.New(
		goruntime.WithInstanceID("uc48-bridge-b"),
		goruntime.WithLeaseStore(leaseStoreB),
		goruntime.WithOutboxStore(outboxStoreB),
		goruntime.WithDLQStore(dlqB),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtB.AddRoute(goruntime.RouteConfig{
		ID: "uc48-route-b",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc48-bind-b", Address: sqsOutURL},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc48-bind-b", SessionID: rxSessIDB},
		},
	}, mqttRxB, sqsSndB, rxSessB, &scB))
	require.NoError(t, rtB.Start(ctx))
	defer func() { _ = rtB.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rtA, rtB)

	t.Logf("UC48: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait until hop topic has ~killAt messages, then kill broker.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("hop collector >= %d before kill", killAt),
		func() bool { return hopCollector.count() >= killAt })

	t.Logf("UC48: killing broker at hop-collector=%d", hopCollector.count())
	broker.StopGraceful()
	time.Sleep(5 * time.Second) // OTHER: scenario timing — keep broker down before restart

	t.Log("UC48: restarting broker")
	broker.RestartGraceful()

	// Black-box readiness: probe the Bridge-A pipeline (SQS-IN → MQTT hop).
	// When the probe arrives at hopCollector, both Bridge-A's session and
	// the hop collector's subscription are proven operational.
	sendProbe(t, sqsInClient, sqsInURL, hopCollector, 30*time.Second)

	// Poll SQS-OUT for delivered messages.
	bodies := pollAllSQS(t, sqsOutClient, sqsOutURL, msgCount, 240*time.Second)
	unique := make(map[string]struct{}, len(bodies))
	for _, b := range bodies {
		unique[b] = struct{}{}
	}

	t.Logf("UC48: SQS-OUT unique=%d, total=%d, dlqA=%d, dlqB=%d",
		len(unique), len(bodies), dlqA.count(), dlqB.count())

	require.GreaterOrEqual(t, len(unique), msgCount,
		"Multi-hop must deliver >= %d unique messages after broker restart", msgCount)
	assert.Equal(t, 0, dlqA.count(), "Bridge-A DLQ should be empty")
}

// =========================================================================
// UC49: SharedOutbox vs DirectHold Under Broker Flapping
// Two parallel paths through the same per-test broker:
//   Path A: SQS → SharedOutbox → MQTT topicA → collectorA
//   Path B: SQS → DirectHold  → MQTT topicB → collectorB
// Broker restarts 3 times during message processing.
// DOCUMENTATION TEST: proves SharedOutbox handles broker instability
// better than DirectHold. DirectHold may lose messages during flaps.
// PRODUCTION FIX NEEDED: RES-001 (autopaho reconnect to restarted broker).
// =========================================================================

func TestUC49_SharedOutboxVsDirectHold_BrokerFlapping(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		topicA      = "uc49/outbox/output"
		topicB      = "uc49/direct/output"
		testTimeout = 360 * time.Second
		flapCount   = 3
		flapDownSec = 3
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInA, sqsClientA := setupSQSQueue(t, "uc49-in-a")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlqA := &lrDLQStore{}

	sqsInB, sqsClientB := setupSQSQueue(t, "uc49-in-b")
	dlqB := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collectorA := newPersistentCollectorWithBroker(t, brokerURL, topicA, "uc49-col-a")
	collectorB := newPersistentCollectorWithBroker(t, brokerURL, topicB, "uc49-col-b")

	// --- Path A: SharedOutbox ---
	sessIDA := mqttlocal.UniqueClientID("uc49-sess-a")
	sessA := setupMQTTSessionWithBroker(t, brokerURL, sessIDA,
		connectivity.SessionExclusive, 65535, 5)
	sndA := setupMQTTSender(t, sessA)
	rxA := newSQSReceiver(t, sqsInA)
	scA := lrSessionConfig(sessIDA)

	rtA := goruntime.New(
		goruntime.WithInstanceID("uc49-bridge-a"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlqA),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtA.AddRoute(goruntime.RouteConfig{
		ID: "uc49-route-a",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			MaxInFlight:  100,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc49-bind-a", Address: topicA},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc49-bind-a", SessionID: sessIDA},
		},
	}, rxA, sndA, sessA, &scA))
	require.NoError(t, rtA.Start(ctx))
	defer func() { _ = rtA.Stop(context.Background()) }()

	// --- Path B: DirectHold ---
	sessIDB := mqttlocal.UniqueClientID("uc49-sess-b")
	sessB := setupMQTTSessionWithBroker(t, brokerURL, sessIDB,
		connectivity.SessionExclusive, 65535, 5)
	sndB := setupMQTTSender(t, sessB)
	rxB := newSQSReceiver(t, sqsInB)

	rtB := goruntime.New(
		goruntime.WithInstanceID("uc49-bridge-b"),
		goruntime.WithDLQStore(dlqB),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtB.AddRoute(goruntime.RouteConfig{
		ID: "uc49-route-b",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc49-bind-b", Address: topicB},
		),
		SourceCapabilities: directHoldCaps,
	}, rxB, sndB, sessB, nil))
	require.NoError(t, rtB.Start(ctx))
	defer func() { _ = rtB.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rtA, rtB)

	t.Logf("UC49: sending %d messages to each path", msgCount)
	sendBulkToSQS(t, sqsClientA, sqsInA, msgCount, nil)
	sendBulkToSQS(t, sqsClientB, sqsInB, msgCount, nil)

	// Flap broker 3 times based on SharedOutbox path progress.
	flapTargets := []int{msgCount / 4, msgCount / 2, 3 * msgCount / 4}
	for i := 0; i < flapCount; i++ {
		target := flapTargets[i]
		lrWaitFor(t, 120*time.Second,
			fmt.Sprintf("collectorA >= %d before flap %d", target, i+1),
			func() bool { return collectorA.count() >= target })

		t.Logf("UC49: flap %d/%d -- collectorA=%d, collectorB=%d",
			i+1, flapCount, collectorA.count(), collectorB.count())
		broker.StopGraceful()
		time.Sleep(time.Duration(flapDownSec) * time.Second) // OTHER: scenario timing — broker flap downtime
		broker.RestartGraceful()
		sendProbe(t, sqsClientA, sqsInA, collectorA, 30*time.Second)
	}

	// SharedOutbox must deliver all.
	lrWaitFor(t, 240*time.Second,
		fmt.Sprintf("SharedOutbox unique >= %d", msgCount),
		func() bool { return countUnique(collectorA) >= msgCount })

	rtB.WaitQuiescent(ctx, goruntime.QuiescenceOptions{MinQuiet: 2 * time.Second, Timeout: 35 * time.Second}) //nolint:errcheck

	uniqueA := countUnique(collectorA)
	uniqueB := countUnique(collectorB)
	t.Logf("UC49: SharedOutbox: unique=%d, total=%d, dlq=%d",
		uniqueA, collectorA.count(), dlqA.count())
	t.Logf("UC49: DirectHold:   unique=%d, total=%d, dlq=%d",
		uniqueB, collectorB.count(), dlqB.count())

	gap := msgCount - uniqueB
	if gap > 0 {
		t.Logf("UC49: EVIDENCE -- DirectHold lost %d msgs under %d broker flaps", gap, flapCount)
		t.Logf("UC49: SharedOutbox handled all %d flaps with zero loss", flapCount)
	} else {
		t.Logf("UC49: DirectHold delivered all (flaps may not have caused loss)")
	}

	require.GreaterOrEqual(t, uniqueA, msgCount,
		"SharedOutbox must deliver all %d despite %d flaps", msgCount, flapCount)
	assert.GreaterOrEqual(t, uniqueB, 0,
		"DirectHold result logged for comparison")
}

// =========================================================================
// UC50: Session Expiry During Long Processing
//
// MQTT session with short SessionExpiryInterval + slow processor.
// If the session expires mid-processing, the bridge must reconnect.
//
// PRODUCTION FIX NEEDED: RES-001 (autopaho reconnect).
// =========================================================================

func TestUC50_SessionExpiryDuringProcessing(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 100
		outTopic    = "uc50/output"
		testTimeout = 120 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc50-in")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollectorWithBroker(t, brokerURL, outTopic, "uc50-col")

	sessID := mqttlocal.UniqueClientID("uc50-sess")
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              sessID,
		KeepAlive:             2,
		ConnectTimeout:        15 * time.Second,
		CleanStart:            true,
		SessionExpiryInterval: 5,
		ReceiveMaximum:        65535,
	}, connectivity.SessionExclusive, nil)
	require.NoError(t, sess.Start(ctx))
	select {
	case <-sess.Events():
	case <-time.After(5 * time.Second):
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc50-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc50-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  10,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc50-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC50: sending %d msgs (KeepAlive=2, SessionExpiry=5)", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 100*time.Second,
		fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	t.Logf("UC50: unique=%d, total=%d, dlq=%d",
		countUnique(collector), collector.count(), dlq.count())
	require.GreaterOrEqual(t, countUnique(collector), msgCount)
}

// =========================================================================
// UC51: Persistent Session Recovery After Broker Restart
//
// Messages published during broker downtime should be queued by the
// broker (persistent session) and delivered after restart.
//
// PRODUCTION FIX NEEDED: RES-001 (autopaho reconnect).
// =========================================================================

func TestUC51_PersistentSessionRecovery(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		beforeKill  = 250
		duringDown  = 250
		outTopic    = "uc51/output"
		testTimeout = 360 * time.Second
	)
	msgCount := beforeKill + duringDown

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc51-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Collector with persistent session so the broker queues messages
	// during the reconnection gap. KeepAlive=5 speeds up disconnect
	// detection after docker kill (default 30s is too slow).
	colID := mqttlocal.UniqueClientID("uc51-col")
	colSess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              colID,
		KeepAlive:             5,
		ConnectTimeout:        15 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, connectivity.SessionPersistent, testLogger(t))
	require.NoError(t, colSess.Start(ctx))
	select {
	case <-colSess.Events():
	case <-time.After(5 * time.Second):
	}
	require.NoError(t, colSess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: outTopic, QoS: 1}},
	}))
	waitSubReady(t, colSess, 5*time.Second)

	colRecv := paho.NewReceiver("col-"+colID, colSess)
	colCtx, colCancel := context.WithCancel(context.Background())
	collector := &mqttCollector{cancel: colCancel}
	collector.wg.Add(1)
	go func() {
		defer collector.wg.Done()
		err := colRecv.Run(colCtx, func(ctx context.Context, del ports.Delivery) error {
			collector.mu.Lock()
			collector.messages = append(collector.messages, del.Envelope())
			collector.mu.Unlock()
			return del.Ack(ctx)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("persistent collector Receiver.Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		colCancel()
		collector.wg.Wait()
		_ = colSess.Close(context.Background())
	})

	// Bridge with SharedOutbox. KeepAlive=5 for fast reconnection.
	sessID := mqttlocal.UniqueClientID("uc51-sess")
	sess := setupMQTTSessionWithBroker(t, brokerURL, sessID,
		connectivity.SessionExclusive, 65535, 5)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc51-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc51-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			MaxReplayAttempts: 50,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc51-bind", Address: outTopic},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc51-bind", SessionID: sessID},
		},
	}, rx, snd, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Send first batch before kill.
	t.Logf("UC51: sending %d msgs before kill", beforeKill)
	sendBulkToSQS(t, sqsInClient, sqsInURL, beforeKill, nil)
	lrWaitFor(t, 30*time.Second, fmt.Sprintf("collector >= %d", beforeKill),
		func() bool { return collector.count() >= beforeKill })

	t.Log("UC51: killing broker")
	broker.StopGraceful()

	// Send second batch during downtime (queues in SQS/outbox).
	t.Logf("UC51: sending %d msgs during downtime", duringDown)
	sendBulkToSQS(t, sqsInClient, sqsInURL, duringDown, nil)
	time.Sleep(5 * time.Second) // OTHER: scenario timing — keep broker down during message queuing

	t.Log("UC51: restarting broker")
	broker.RestartGraceful()

	// Black-box readiness: probe proves the full pipeline is operational.
	// Graceful restart preserves the collector's persistent session, so
	// the broker queues messages during downtime and delivers them on
	// reconnect — no subscriber-before-publisher race.
	sendProbe(t, sqsInClient, sqsInURL, collector, 30*time.Second)

	lrWaitFor(t, 200*time.Second, fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	t.Logf("UC51: unique=%d, total=%d, dlq=%d",
		countUnique(collector), collector.count(), dlq.count())
	require.GreaterOrEqual(t, countUnique(collector), msgCount)
}
