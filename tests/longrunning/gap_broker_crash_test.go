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

// ═══════════════════════════════════════════════════════════════════════════
// Gap Tests: Broker Crash Recovery (Category 3 — Broker Resilience)
//
// Unlike UC42/UC43 which use StopGraceful/RestartGraceful (docker stop +
// docker start — preserves container), these tests use Stop/Restart
// (docker kill + docker run — destroys all broker state).
//
// Summary:
// ┌──────┬────────────────────────────────────┬──────────┐
// │ ID   │ Description                        │ Status   │
// ├──────┼────────────────────────────────────┼──────────┤
// │ BC-1 │ Hard crash + SharedOutbox replay   │ PENDING  │
// │ BC-2 │ KeepAlive disconnect detection     │ PENDING  │
// └──────┴────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_BrokerHardCrash_SharedOutbox validates that SharedOutbox replays
// ALL messages after a broker hard kill (total state loss).
//
// Scenario:
// ───────────────────────────────────────────────
//   SQS ──▶ [Bridge] ──▶ [Outbox] ──▶ [MQTT Broker]
//                                          │
//                                    docker kill
//                                    (total state loss)
//                                          │
//                                    docker run
//                                    (fresh broker)
//                                          │
//                              outbox replay fills gap
//                                          │
//                                          ▼
//                                   collector >= 2000
// ───────────────────────────────────────────────
//
// Key difference from UC42: UC42 uses StopGraceful/RestartGraceful which
// preserves the Docker container. This test uses Stop/Restart which kills
// the process and creates a fresh container with no MQTT session state.
//
// Test Parameters:
//   - Messages: 2000
//   - KeepAlive: 5 (fast disconnect detection)
//   - AckAfter: AckAfterOutboxPersist
//   - Broker: per-test instance with persistence
//
// Assertions:
//   - collector >= 2000 (outbox replayed everything)
//   - DLQ empty
func TestGAP_BrokerHardCrash_SharedOutbox(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "gap-bc1/output"
		testTimeout = 300 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Per-test broker with persistence.
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithPersistence(true),
		mqttlocal.WithMaxInflightMessages(65534),
		mqttlocal.WithMaxQueuedMessages(65534),
	)
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-bc1-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	// Persistent collector survives broker restart.
	collector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "gap-bc1-col")

	sessID := mqttlocal.UniqueClientID("gap-bc1-sess")
	sess := newMQTTSessionWithBroker(t, brokerURL, sessID, domain.SessionExclusive, 65534, 5)
	snd := setupMQTTSender(t, sess)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-bc1"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-bc1-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "bc1-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "bc1-bind", SessionID: sessID},
		},
	}, newSQSReceiver(t, sqsInURL), snd, sess, &sc))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 15*time.Second, rt)

	t.Logf("GAP-BC1: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for partial delivery.
	lrWaitFor(t, 60*time.Second, "collector >= 500",
		func() bool { return collector.count() >= 500 })
	t.Logf("GAP-BC1: collector at %d — hard killing broker", collector.count())

	// Hard kill: docker kill (no MQTT disconnect packet, total state loss).
	broker.Stop()
	t.Log("GAP-BC1: broker killed — waiting 5s")
	time.Sleep(5 * time.Second)

	// Restart: docker run (fresh container, no session state).
	broker.Restart()
	t.Log("GAP-BC1: broker restarted — waiting for pipeline recovery")

	// Use sendProbe to verify end-to-end pipeline recovery.
	sendProbe(t, sqsInClient, sqsInURL, collector, 60*time.Second)
	t.Logf("GAP-BC1: pipeline recovered — collector at %d", collector.count())

	// Wait for outbox replay to complete delivery.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	delivered := collector.count()
	t.Logf("GAP-BC1: delivered=%d/%d, dlq=%d", delivered, msgCount, dlq.count())

	assert.GreaterOrEqual(t, delivered, msgCount,
		"outbox must replay all %d messages after broker hard crash", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	require.NoError(t, rt.Stop(context.Background()))
}

// TestGAP_BrokerDisconnect_KeepAliveDetection validates that the bridge
// detects a broken MQTT connection and recovers after broker restart,
// using SharedOutbox for guaranteed replay.
//
// Scenario:
// ───────────────────────────────────────────────
//   SQS ──▶ [Bridge] ──▶ [Outbox] ──▶ [MQTT]
//                                        │
//                                   docker kill
//                                   docker run
//                                        │
//                                  outbox replay
//                                        ▼
//                                 collector >= 1000
// ───────────────────────────────────────────────
//
// Assertions:
//   - Pipeline recovers after broker hard kill
//   - All messages delivered via outbox replay
//   - DLQ empty
func TestGAP_BrokerDisconnect_KeepAliveDetection(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 1000
		outTopic    = "gap-bc2/output"
		testTimeout = 300 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithPersistence(true),
		mqttlocal.WithMaxInflightMessages(65534),
		mqttlocal.WithMaxQueuedMessages(65534),
	)
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-bc2-in")
	leaseStore, outboxStore := setupDynamoStoresForRestart(t)
	dlq := &lrDLQStore{}

	collector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "gap-bc2-col")

	sessID := mqttlocal.UniqueClientID("gap-bc2-sess")
	sess := newMQTTSessionWithBroker(t, brokerURL, sessID, domain.SessionExclusive, 65534, 3)
	snd := setupMQTTSender(t, sess)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-bc2"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-bc2-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "bc2-bind", Address: outTopic}),
		Bindings: []domain.DestinationBinding{
			{ID: "bc2-bind", SessionID: sessID},
		},
	}, newSQSReceiver(t, sqsInURL), snd, sess, &sc))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 15*time.Second, rt)

	t.Logf("GAP-BC2: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 30*time.Second, "collector >= 300",
		func() bool { return collector.count() >= 300 })
	t.Logf("GAP-BC2: collector at %d — hard killing broker (KeepAlive=3)", collector.count())

	killTime := time.Now()
	broker.Stop()
	t.Log("GAP-BC2: broker killed — waiting 3s")
	time.Sleep(3 * time.Second)
	broker.Restart()
	t.Log("GAP-BC2: broker restarted")

	// Use sendProbe to verify full pipeline recovery.
	sendProbe(t, sqsInClient, sqsInURL, collector, 60*time.Second)
	recoveryTime := time.Since(killTime)
	t.Logf("GAP-BC2: pipeline recovered in %v", recoveryTime)

	// Wait for outbox replay to complete all messages.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	delivered := collector.count()
	t.Logf("GAP-BC2: delivered=%d/%d, dlq=%d", delivered, msgCount, dlq.count())

	assert.GreaterOrEqual(t, delivered, msgCount,
		"all %d messages should be delivered after broker restart", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	require.NoError(t, rt.Stop(context.Background()))
}
