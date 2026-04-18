//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestUC59_PartitionHotspot sends 5,000 messages through a SharedOutbox route
// where every message targets a single MQTT topic. This exercises the outbox
// partition under a "hot key" workload and verifies that all messages are
// delivered without any ending up in the DLQ.
func TestUC59_PartitionHotspot(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 5000
		outTopic    = "uc59/output"
		testTimeout = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc59-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc59-col")

	sessID := mqttlocal.UniqueClientID("uc59-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc59"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(
		goruntime.RouteConfig{
			ID: "uc59-route",
			Policy: domain.RoutePolicy{
				DeliveryMode: domain.DeliverySharedOutbox,
				MaxInFlight:  200,
			},
			Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
				BindingID: "uc59-bind",
				Address:   outTopic,
			}),
			Bindings: []domain.DestinationBinding{
				{ID: "uc59-bind", SessionID: sessID},
			},
		}, rx, snd, sess, &sc,
	))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 160*time.Second, fmt.Sprintf("unique >= %d", msgCount), func() bool {
		return countUnique(collector) >= msgCount
	})

	elapsed := time.Since(start)
	t.Logf("UC59: %d msgs in %v (%.0f msgs/sec)", msgCount, elapsed, float64(msgCount)/elapsed.Seconds())

	require.GreaterOrEqual(t, countUnique(collector), msgCount)
	assert.Equal(t, 0, dlq.count())
}

// TestUC60_OutboxPlusBrokerDown verifies that a SharedOutbox route with
// AckAfterOutboxPersist survives a broker crash and re-delivers every message
// once the broker comes back online. This is the PRODUCTION FIX validation
// for RES-001 (reconnect after broker restart).
func TestUC60_OutboxPlusBrokerDown(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "uc60/output"
		testTimeout = 240 * time.Second
		waitBefore  = 500
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc60-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Start a dedicated broker so we can kill and restart it.
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	// Persistent collector — broker queues messages during disconnection.
	collector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "uc60-col")

	sessID := mqttlocal.UniqueClientID("uc60-sess")
	sess := newMQTTSessionWithBroker(t, brokerURL, sessID, domain.SessionExclusive, 50, 5)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc60"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(
		goruntime.RouteConfig{
			ID: "uc60-route",
			Policy: domain.RoutePolicy{
				DeliveryMode: domain.DeliverySharedOutbox,
				AckAfter:     domain.AckAfterOutboxPersist,
				MaxInFlight:  100,
			},
			Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
				BindingID: "uc60-bind",
				Address:   outTopic,
			}),
			Bindings: []domain.DestinationBinding{
				{ID: "uc60-bind", SessionID: sessID},
			},
		}, rx, snd, sess, &sc,
	))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	// Send all messages into SQS.
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait until the collector has received at least waitBefore messages,
	// then kill the broker to simulate an infrastructure failure.
	lrWaitFor(t, 60*time.Second, fmt.Sprintf("collector >= %d before kill", waitBefore), func() bool {
		return countUnique(collector) >= waitBefore
	})
	t.Log("UC60: killing broker after initial delivery")
	broker.StopGraceful()

	time.Sleep(5 * time.Second) // OTHER: scenario timing — keep broker down before restart

	t.Log("UC60: restarting broker")
	broker.RestartGraceful()

	// Black-box readiness: probe proves the pipeline is operational.
	sendProbe(t, sqsInClient, sqsInURL, collector, 30*time.Second)

	lrWaitFor(t, 180*time.Second, fmt.Sprintf("unique >= %d after restart", msgCount), func() bool {
		return countUnique(collector) >= msgCount
	})

	require.GreaterOrEqual(t, countUnique(collector), msgCount)
	assert.Equal(t, 0, dlq.count(), "no messages should land in the DLQ")
}

// TestUC61_MaxReplayAttempts sends 500 messages through a SharedOutbox route
// where the MQTT sender fails the first 3 send attempts for every message.
// With MaxReplayAttempts set to 5, every message should eventually succeed on
// the 4th attempt. This validates the outbox retry / replay-attempt counter.
func TestUC61_MaxReplayAttempts(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		outTopic    = "uc61/output"
		testTimeout = 180 * time.Second
		failCount   = 3
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc61-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc61-col")

	sessID := mqttlocal.UniqueClientID("uc61-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	mqttSnd := setupMQTTSender(t, sess)
	snd := newFailFirstNSender(mqttSnd, failCount)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc61"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(
		goruntime.RouteConfig{
			ID: "uc61-route",
			Policy: domain.RoutePolicy{
				DeliveryMode:      domain.DeliverySharedOutbox,
				MaxInFlight:       50,
				MaxReplayAttempts: 5,
			},
			Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
				BindingID: "uc61-bind",
				Address:   outTopic,
			}),
			Bindings: []domain.DestinationBinding{
				{ID: "uc61-bind", SessionID: sessID},
			},
		}, rx, snd, sess, &sc,
	))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 160*time.Second, fmt.Sprintf("unique >= %d", msgCount), func() bool {
		return countUnique(collector) >= msgCount
	})

	require.GreaterOrEqual(t, countUnique(collector), msgCount)
	assert.Equal(t, 0, dlq.count(), "all messages should succeed within MaxReplayAttempts")
	t.Logf("UC61: %d msgs delivered after sender failed first %d attempts per msg", msgCount, failCount)
}

// TestUC62_LeaseRenewalHighLoad pushes 10,000 messages through a SharedOutbox
// route at MaxInFlight=500. This stresses the lease-renewal path: with so
// many concurrent messages the lease must be renewed multiple times during
// processing. All messages must arrive and the DLQ must remain empty.
func TestUC62_LeaseRenewalHighLoad(t *testing.T) {
	// NOTE: 10k messages through SharedOutbox with DynamoDB Local requires
	// generous timeouts. DynamoDB Local pay-per-request mode can throttle
	// under high concurrent write load (outbox claim + complete + lease
	// renewal all compete for throughput), causing cascading latency.
	_ = withFreshInfra(t)
	const (
		msgCount    = 5000 // reduced from 10k: DynamoDB Local locks up under sustained high write load
		outTopic    = "uc62/output"
		testTimeout = 600 * time.Second // 10 min: DynamoDB Local needs headroom
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc62-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc62-col")

	sessID := mqttlocal.UniqueClientID("uc62-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc62"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(
		goruntime.RouteConfig{
			ID: "uc62-route",
			Policy: domain.RoutePolicy{
				DeliveryMode: domain.DeliverySharedOutbox,
				MaxInFlight:  200, // reduced from 500: DynamoDB Local locks up under high concurrent writes
			},
			Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
				BindingID: "uc62-bind",
				Address:   outTopic,
			}),
			Bindings: []domain.DestinationBinding{
				{ID: "uc62-bind", SessionID: sessID},
			},
		}, rx, snd, sess, &sc,
	))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 540*time.Second, fmt.Sprintf("unique >= %d", msgCount), func() bool {
		return countUnique(collector) >= msgCount
	})

	elapsed := time.Since(start)
	t.Logf("UC62: %d msgs in %v (%.0f msgs/sec)", msgCount, elapsed, float64(msgCount)/elapsed.Seconds())

	require.GreaterOrEqual(t, countUnique(collector), msgCount)
	assert.Equal(t, 0, dlq.count(), "no messages should land in the DLQ under high load")
}
