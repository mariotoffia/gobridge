//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"runtime"
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
// UC30: DLQ All Poison
//
// 5,000 messages all with poison=true header. All -> DLQ.
// Bridge stays healthy.
// =========================================================================

func TestUC30_DLQ_AllPoison(t *testing.T) {
	const (
		msgCount = 5000
		timeout  = 120 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc30-in")
	collector := newMQTTCollector(t, "uc30/output/data", "uc30-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc30-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc30-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc30-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        100,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{&poisonProcessor{}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc30/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// All messages are poison.
	sendBulkToSQS(t, inClient, inQueueURL, msgCount, func(i int) map[string]string {
		return map[string]string{"poison": "true"}
	})

	lrWaitFor(t, timeout, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlqStore.count() >= msgCount
	})

	time.Sleep(2 * time.Second)

	gotDLQ := dlqStore.count()
	gotMQTT := collector.count()

	require.Equal(t, msgCount, gotDLQ,
		"DLQ should have %d entries, got %d", msgCount, gotDLQ)
	require.Equal(t, 0, gotMQTT,
		"MQTT collector should have 0 messages, got %d", gotMQTT)

	// Bridge must still be healthy.
	require.True(t, rt.Healthy(), "bridge should still be healthy")

	t.Logf("UC30: DLQ=%d, MQTT=%d, healthy=%v", gotDLQ, gotMQTT, rt.Healthy())
}

// =========================================================================
// UC31: Outbox Replay Exhaustion
//
// 100 messages via SharedOutbox with MaxReplayAttempts=3.
// alwaysFailSender causes every send to fail. After replay exhaustion,
// all messages land in DLQ.
// =========================================================================

func TestUC31_OutboxReplay_Exhaustion(t *testing.T) {
	const (
		msgCount = 100
		timeout  = 120 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc31-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlqStore := &lrDLQStore{}

	sessionID := mqttlocal.UniqueClientID("uc31-session")
	sess := setupMQTTSession(t, sessionID, domain.SessionExclusive)
	failSender := &alwaysFailSender{}
	sqsRx := newSQSReceiver(t, inQueueURL)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc31-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc31-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliverySharedOutbox,
			MaxReplayAttempts:  3,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc31-bind", Address: "uc31/output/data"},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc31-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, failSender, sess, &sc))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	lrWaitFor(t, timeout, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlqStore.count() >= msgCount
	})

	time.Sleep(2 * time.Second)

	gotDLQ := dlqStore.count()
	require.GreaterOrEqual(t, gotDLQ, msgCount,
		"DLQ should have at least %d entries, got %d", msgCount, gotDLQ)

	t.Logf("UC31: DLQ=%d (all replay-exhausted)", gotDLQ)
}

// =========================================================================
// UC32: Graceful Shutdown Under Load
//
// Send 3,000 messages. After collector has >= 500, call rt.Stop().
// Verify: Stop returns nil, IsRunning=false, collector >= 500,
// no goroutine leak.
// =========================================================================

func TestUC32_GracefulShutdown_UnderLoad(t *testing.T) {
	const (
		msgCount    = 3000
		minReceived = 500
		timeout     = 60 * time.Second
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc32-in")
	collector := newMQTTCollector(t, "uc32/output/data", "uc32-col")
	time.Sleep(300 * time.Millisecond)

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc32-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc32-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc32-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        100,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc32/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, rt.Start(ctx))

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	// Wait until at least minReceived messages are collected.
	lrWaitFor(t, 30*time.Second,
		fmt.Sprintf("collector >= %d", minReceived), func() bool {
			return collector.count() >= minReceived
		})

	beforeStop := collector.count()
	t.Logf("UC32: collector has %d messages before Stop", beforeStop)

	// Record goroutine count before stop.
	goroutinesBefore := runtime.NumGoroutine()

	// Graceful shutdown.
	stopErr := rt.Stop(context.Background())
	require.NoError(t, stopErr, "rt.Stop should return nil")
	require.False(t, rt.IsRunning(), "rt.IsRunning should be false after Stop")

	afterStop := collector.count()
	require.GreaterOrEqual(t, afterStop, minReceived,
		"collector should have >= %d messages after stop, got %d",
		minReceived, afterStop)

	// Check for goroutine leak: allow a grace period for cleanup.
	time.Sleep(1 * time.Second)
	goroutinesAfter := runtime.NumGoroutine()

	// goroutinesBefore was taken while bridge was running, so after should
	// be lower or similar. Allow modest tolerance for test infrastructure.
	assert.LessOrEqual(t, goroutinesAfter, goroutinesBefore+5,
		"goroutine leak detected: before=%d, after=%d",
		goroutinesBefore, goroutinesAfter)

	t.Logf("UC32: Stop=nil, IsRunning=%v, collector=%d, goroutines %d->%d",
		rt.IsRunning(), afterStop, goroutinesBefore, goroutinesAfter)
}
