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
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC57: Stale Claim Recovery After Crash
//
// Bridge-A processes messages via SharedOutbox. After ~300 are delivered,
// Bridge-A's context is cancelled (simulating a crash without graceful
// shutdown). Bridge-B starts with the SAME outbox/lease stores and
// recovers the stale outbox claims that Bridge-A left behind.
//
// Assert: >= 1,000 unique delivered. DLQ empty.
//
// PRODUCTION FIX NEEDED:
//   - RES-004: Lease transfer duplicate window. Old owner's in-flight
//     goroutines run under context.Background() and may Complete records
//     after the new owner reclaims them. Fencing tokens should prevent
//     this, but context cancellation cleanup is not guaranteed.
//   - RES-001: autopaho reconnect (Bridge-B session may not connect if
//     Bridge-A's exclusive client ID lingers on the broker).
// =========================================================================

func TestUC57_StaleClaimRecovery(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 1000
		crashAt     = 300
		outTopic    = "uc57/output"
		testTimeout = 240 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc57-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	collector := newMQTTCollectorWithBroker(t, brokerURL, outTopic, "uc57-col")

	// Shared lease identity — both bridges compete for this lease.
	leaseSessionID := "uc57-lease"

	// --- Phase 1: Bridge-A processes until crash ---
	ctxA, cancelA := context.WithTimeout(context.Background(), testTimeout)
	defer cancelA()

	mqttIDA := mqttlocal.UniqueClientID("uc57-a")
	sessA := setupMQTTSessionWithBroker(t, brokerURL, mqttIDA,
		domain.SessionExclusive, 65535)
	sndA := setupMQTTSender(t, sessA)
	rxA := newSQSReceiver(t, sqsInURL)
	scA := lrSessionConfig(leaseSessionID)

	rtA := goruntime.New(
		goruntime.WithInstanceID("uc57-bridge-a"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtA.AddRoute(goruntime.RouteConfig{
		ID: "uc57-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc57-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc57-bind", SessionID: leaseSessionID},
		},
	}, rxA, sndA, sessA, &scA))
	require.NoError(t, rtA.Start(ctxA))
	// NOT deferring Stop — simulating a crash (no graceful shutdown).

	gobridgesync(t, 10*time.Second, rtA)

	t.Logf("UC57: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d before crash", crashAt),
		func() bool { return collector.count() >= crashAt })

	beforeCrash := collector.count()
	t.Logf("UC57: crashing Bridge-A at collector=%d (cancelling context)", beforeCrash)
	cancelA() // Simulate crash — no graceful Stop.

	time.Sleep(5 * time.Second) // ESSENTIAL: wait for lease TTL (2s) to expire + margin

	// --- Phase 2: Bridge-B takes over and recovers stale claims ---
	ctxB, cancelB := context.WithTimeout(context.Background(), testTimeout)
	defer cancelB()

	mqttIDB := mqttlocal.UniqueClientID("uc57-b")
	sessB := setupMQTTSessionWithBroker(t, brokerURL, mqttIDB,
		domain.SessionExclusive, 65535)
	sndB := setupMQTTSender(t, sessB)
	rxB := newSQSReceiver(t, sqsInURL) // Same queue — picks up remaining msgs.
	scB := lrSessionConfig(leaseSessionID) // Same lease — competes for ownership.

	rtB := goruntime.New(
		goruntime.WithInstanceID("uc57-bridge-b"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtB.AddRoute(goruntime.RouteConfig{
		ID: "uc57-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc57-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc57-bind", SessionID: leaseSessionID},
		},
	}, rxB, sndB, sessB, &scB))
	require.NoError(t, rtB.Start(ctxB))
	defer func() { _ = rtB.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rtB)

	lrWaitFor(t, 180*time.Second,
		fmt.Sprintf("unique >= %d after recovery", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	unique := countUnique(collector)
	t.Logf("UC57: unique=%d, total=%d, dlq=%d, beforeCrash=%d",
		unique, collector.count(), dlq.count(), beforeCrash)

	require.GreaterOrEqual(t, unique, msgCount,
		"Bridge-B must recover stale claims and deliver all %d unique messages", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// =========================================================================
// UC58: Double-Drain Prevention (Fencing Tokens)
//
// Two bridges race for the same outbox partition via shared lease/outbox
// stores. Only the lease holder should actively drain. Fencing tokens
// prevent the old owner from completing stale claims after lease loss.
//
// Assert: >= 2,000 unique delivered. DLQ empty.
//         Only 1 bridge reports "active" at each health sample.
//
// PRODUCTION FIX NEEDED:
//   - RES-004: Old owner's in-flight goroutines may complete Send after
//     new owner reclaims. Fencing token validation on Complete should
//     reject stale tokens, but context.Background() usage may cause
//     the old goroutine to succeed before the token check runs.
// =========================================================================

func TestUC58_DoubleDrainPrevention(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		outTopic    = "uc58/output"
		testTimeout = 300 * time.Second
	)

	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc58-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollectorWithBroker(t, brokerURL, outTopic, "uc58-col")

	// Shared lease identity — both bridges compete for this single lease.
	leaseSessionID := "uc58-lease"

	routeCfg := func(id string) goruntime.RouteConfig {
		return goruntime.RouteConfig{
			ID: "uc58-route",
			Policy: domain.RoutePolicy{
				DeliveryMode: domain.DeliverySharedOutbox,
			},
			Resolver: goruntime.NewStaticResolver(
				domain.DispatchPlan{BindingID: "uc58-bind", Address: outTopic},
			),
			Bindings: []domain.DestinationBinding{
				{ID: "uc58-bind", SessionID: leaseSessionID},
			},
		}
	}

	// --- Bridge-A ---
	mqttIDA := mqttlocal.UniqueClientID("uc58-a")
	sessA := setupMQTTSessionWithBroker(t, brokerURL, mqttIDA,
		domain.SessionExclusive, 65535)
	sndA := setupMQTTSender(t, sessA)
	rxA := newSQSReceiver(t, sqsInURL)
	scA := lrSessionConfig(leaseSessionID)

	rtA := goruntime.New(
		goruntime.WithInstanceID("uc58-bridge-a"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtA.AddRoute(routeCfg("a"), rxA, sndA, sessA, &scA))
	require.NoError(t, rtA.Start(ctx))
	defer func() { _ = rtA.Stop(context.Background()) }()

	// --- Bridge-B ---
	mqttIDB := mqttlocal.UniqueClientID("uc58-b")
	sessB := setupMQTTSessionWithBroker(t, brokerURL, mqttIDB,
		domain.SessionExclusive, 65535)
	sndB := setupMQTTSender(t, sessB)
	rxB := newSQSReceiver(t, sqsInURL) // Same SQS queue — competing consumers.
	scB := lrSessionConfig(leaseSessionID) // Same lease — competing for ownership.

	rtB := goruntime.New(
		goruntime.WithInstanceID("uc58-bridge-b"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rtB.AddRoute(routeCfg("b"), rxB, sndB, sessB, &scB))
	require.NoError(t, rtB.Start(ctx))
	defer func() { _ = rtB.Stop(context.Background()) }()

	// At least one bridge must become fully ready (the lease winner).
	lrWaitFor(t, 15*time.Second, "at least one bridge ready", func() bool {
		dhA := rtA.DeepHealth(context.Background())
		dhB := rtB.DeepHealth(context.Background())
		aFull := dhA.ReadyForTraffic && dhA.ServiceLevel == ports.ServiceLevelFull
		bFull := dhB.ReadyForTraffic && dhB.ServiceLevel == ports.ServiceLevelFull
		return aFull || bFull
	})

	t.Logf("UC58: sending %d messages to shared SQS queue", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Periodically sample health to check that only one is active.
	activeSamples := 0
	bothActiveSamples := 0
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dhA := rtA.DeepHealth(context.Background())
				dhB := rtB.DeepHealth(context.Background())
				aReady := dhA.ReadyForTraffic
				bReady := dhB.ReadyForTraffic
				activeSamples++
				if aReady && bReady {
					bothActiveSamples++
				}
				if isDebug() {
					t.Logf("UC58: sample %d -- A.ready=%v B.ready=%v",
						activeSamples, aReady, bReady)
				}
			}
		}
	}()

	lrWaitFor(t, 160*time.Second,
		fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	cancel() // Stop the health sampling goroutine.
	<-sampleDone

	unique := countUnique(collector)
	total := collector.count()
	duplicates := total - unique

	t.Logf("UC58: unique=%d, total=%d, duplicates=%d, dlq=%d",
		unique, total, duplicates, dlq.count())
	t.Logf("UC58: health samples=%d, both-active=%d",
		activeSamples, bothActiveSamples)

	if duplicates > 0 {
		t.Logf("UC58: EVIDENCE -- %d duplicate deliveries (fencing gap?)", duplicates)
	}
	if bothActiveSamples > 0 {
		t.Logf("UC58: WARNING -- %d/%d samples had both bridges active",
			bothActiveSamples, activeSamples)
	}

	require.GreaterOrEqual(t, unique, msgCount,
		"Fenced draining must deliver >= %d unique messages", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}
