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
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
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
//
//	SQS ──▶ [Bridge] ──▶ [Outbox] ──▶ [MQTT Broker]
//	                                       │
//	                                 docker kill
//	                                 (total state loss)
//	                                       │
//	                                 docker run
//	                                 (fresh broker)
//	                                       │
//	                           outbox replay fills gap
//	                                       │
//	                                       ▼
//	                                collector >= 2000
//
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
		msgCount         = 2000
		preCrashMessages = 500
		outTopic         = "gap-bc1/output"
		testTimeout      = 300 * time.Second
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
	rt, sess := startBrokerCrashRuntime(
		t, ctx, brokerURL, "gap-bc1", sessID, outTopic, sqsInURL, 5,
		leaseStore, outboxStore, dlq,
	)

	t.Logf("GAP-BC1: sending %d messages before crash", preCrashMessages)
	sendBulkToSQS(t, sqsInClient, sqsInURL, preCrashMessages, nil)

	// Establish a settled pre-crash prefix. Killing while MQTT publishes are
	// broker-accepted but not yet observed downstream would test broker data
	// loss, which SharedOutbox cannot replay after a total broker-state loss.
	lrWaitFor(t, 60*time.Second, "collector >= pre-crash messages",
		func() bool { return collector.count() >= preCrashMessages })
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		MinQuiet: 500 * time.Millisecond,
		Timeout:  10 * time.Second,
	}))
	t.Logf("GAP-BC1: collector at %d — hard killing broker", collector.count())

	// Hard kill: docker kill (no MQTT disconnect packet, total state loss).
	broker.Stop()
	lrWaitFor(t, 15*time.Second, "bridge session disconnected after broker kill",
		func() bool { return !sess.Health(context.Background()).Connected })
	t.Logf("GAP-BC1: sending %d messages while broker is down", msgCount-preCrashMessages)
	sendBulkRangeToSQS(t, sqsInClient, sqsInURL,
		preCrashMessages, msgCount-preCrashMessages, nil)
	waitForSQSQueueDrained(t, ctx, sqsInClient, sqsInURL, 60*time.Second)
	lrWaitFor(t, 30*time.Second, "outage batch persisted to outbox", func() bool {
		pending, supported, err := rt.OutboxPending(
			ctx, persistence.OutboxPartitionKey(sessID, ""),
		)
		return err == nil && supported && pending > 0
	})
	require.NoError(t, rt.Stop(context.Background()))

	// Restart the fresh broker, establish the observer, then let a replacement
	// runtime acquire the lease and replay. This removes subscriber/replay races.
	broker.Restart()
	recoveryCollector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "gap-bc1-recovery-col")
	recoveryRT, _ := startBrokerCrashRuntime(
		t, ctx, brokerURL, "gap-bc1-recovery", sessID, outTopic, sqsInURL, 5,
		leaseStore, outboxStore, dlq,
	)
	t.Logf("GAP-BC1: pipeline recovered — recovery collector at %d", recoveryCollector.count())

	// Wait for outbox replay to complete every workload sequence. The probe is
	// deliberately excluded so it cannot mask a missing workload message.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("all %d workload sequences", msgCount),
		func() bool {
			return len(observedSequences(collector, recoveryCollector)) >= msgCount
		})

	delivered := len(observedSequences(collector, recoveryCollector))
	t.Logf("GAP-BC1: delivered=%d/%d, dlq=%d", delivered, msgCount, dlq.count())

	requireExactSequences(t, msgCount, collector, recoveryCollector)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	require.NoError(t, recoveryRT.Stop(context.Background()))
}

// TestGAP_BrokerDisconnect_KeepAliveDetection validates that the bridge
// detects a broken MQTT connection and recovers after broker restart,
// using SharedOutbox for guaranteed replay.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	SQS ──▶ [Bridge] ──▶ [Outbox] ──▶ [MQTT]
//	                                     │
//	                                docker kill
//	                                docker run
//	                                     │
//	                               outbox replay
//	                                     ▼
//	                              collector >= 1000
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - Pipeline recovers after broker hard kill
//   - All messages delivered via outbox replay
//   - DLQ empty
func TestGAP_BrokerDisconnect_KeepAliveDetection(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount         = 1000
		preCrashMessages = 300
		outTopic         = "gap-bc2/output"
		testTimeout      = 300 * time.Second
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
	rt, sess := startBrokerCrashRuntime(
		t, ctx, brokerURL, "gap-bc2", sessID, outTopic, sqsInURL, 3,
		leaseStore, outboxStore, dlq,
	)

	t.Logf("GAP-BC2: sending %d messages before crash", preCrashMessages)
	sendBulkToSQS(t, sqsInClient, sqsInURL, preCrashMessages, nil)

	lrWaitFor(t, 30*time.Second, "collector >= pre-crash messages",
		func() bool { return collector.count() >= preCrashMessages })
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		MinQuiet: 500 * time.Millisecond,
		Timeout:  10 * time.Second,
	}))
	t.Logf("GAP-BC2: collector at %d — hard killing broker (KeepAlive=3)", collector.count())

	killTime := time.Now()
	broker.Stop()
	lrWaitFor(t, 15*time.Second, "KeepAlive detects broker disconnect",
		func() bool { return !sess.Health(context.Background()).Connected })
	t.Logf("GAP-BC2: sending %d messages while broker is down", msgCount-preCrashMessages)
	sendBulkRangeToSQS(t, sqsInClient, sqsInURL,
		preCrashMessages, msgCount-preCrashMessages, nil)
	waitForSQSQueueDrained(t, ctx, sqsInClient, sqsInURL, 60*time.Second)
	lrWaitFor(t, 30*time.Second, "outage batch persisted to outbox", func() bool {
		pending, supported, err := rt.OutboxPending(
			ctx, persistence.OutboxPartitionKey(sessID, ""),
		)
		return err == nil && supported && pending > 0
	})
	require.NoError(t, rt.Stop(context.Background()))

	broker.Restart()
	recoveryCollector := newPersistentCollectorWithBroker(t, brokerURL, outTopic, "gap-bc2-recovery-col")
	recoveryRT, _ := startBrokerCrashRuntime(
		t, ctx, brokerURL, "gap-bc2-recovery", sessID, outTopic, sqsInURL, 3,
		leaseStore, outboxStore, dlq,
	)
	recoveryTime := time.Since(killTime)
	t.Logf("GAP-BC2: pipeline recovered in %v", recoveryTime)

	// Wait for outbox replay to complete every workload sequence. The probe is
	// deliberately excluded so it cannot mask a missing workload message.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("all %d workload sequences", msgCount),
		func() bool {
			return len(observedSequences(collector, recoveryCollector)) >= msgCount
		})

	delivered := len(observedSequences(collector, recoveryCollector))
	t.Logf("GAP-BC2: delivered=%d/%d, dlq=%d", delivered, msgCount, dlq.count())

	requireExactSequences(t, msgCount, collector, recoveryCollector)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	require.NoError(t, recoveryRT.Stop(context.Background()))
}

func startBrokerCrashRuntime(
	t *testing.T,
	ctx context.Context,
	brokerURL, instanceID, sessionID, topic, queueURL string,
	keepAlive uint16,
	leaseStore ports.LeaseStore,
	outboxStore ports.OutboxStore,
	dlq *lrDLQStore,
) (*goruntime.Runtime, *paho.Session) {
	t.Helper()
	sess := newMQTTSessionWithBroker(
		t, brokerURL, sessionID, connectivity.SessionExclusive, 65534, keepAlive,
	)
	sc := lrSessionConfig(sessionID)
	rt := goruntime.New(
		goruntime.WithInstanceID(instanceID),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	bindingID := instanceID + "-binding"
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: instanceID + "-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			AckAfter:     routing.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: bindingID, Address: topic},
		),
		Bindings: []routing.DestinationBinding{{
			ID: bindingID, SessionID: sessionID,
		}},
	}, newSQSReceiver(t, queueURL), setupMQTTSender(t, sess), sess, &sc))
	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 15*time.Second, rt)
	return rt, sess
}

func observedSequences(collectors ...*mqttCollector) map[int]int {
	seen := make(map[int]int)
	for _, collector := range collectors {
		for _, envelope := range collector.getMessages() {
			var seq int
			if matched, _ := fmt.Sscanf(string(envelope.Payload()), `{"seq":%d}`, &seq); matched == 1 {
				seen[seq]++
			}
		}
	}
	return seen
}

func requireExactSequences(t *testing.T, count int, collectors ...*mqttCollector) {
	t.Helper()
	seen := observedSequences(collectors...)
	require.Len(t, seen, count, "workload sequence cardinality")
	for seq := range count {
		assert.Contains(t, seen, seq, "missing workload sequence %d", seq)
	}
}
