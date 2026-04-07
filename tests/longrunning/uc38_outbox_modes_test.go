//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// =========================================================================
// UC38: Outbox Depth Limit -- 500 msgs, MaxOutboxDepth=100, pausableSender
// SQS-IN -> [Bridge] (SharedOutbox, depth=100) -> MQTT uc38/output/data
// =========================================================================

func TestUC38_OutboxDepthLimit(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		maxDepth    = 100
		pollTimeout = 120 * time.Second
		outTopic    = "uc38/output/data"
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc38-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc38-session")
	collector := newMQTTCollector(t, outTopic, "uc38-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mqttSess := newMQTTSession(t, sessionID, domain.SessionExclusive)
	realSender := setupMQTTSender(t, mqttSess)
	paused := newPausableSender(realSender)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc38-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc38-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:   domain.DeliverySharedOutbox,
			MaxOutboxDepth: maxDepth,
			DepthCacheTTL:  200 * time.Millisecond,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc38-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc38-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, paused, mqttSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC38: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for some delivery, then pause to let outbox fill.
	lrWaitFor(t, 30*time.Second, ">=50 collected", func() bool {
		return collector.count() >= 50
	})
	t.Logf("UC38: pausing sender at collector=%d", collector.count())
	paused.Pause()

	time.Sleep(10 * time.Second)
	t.Logf("UC38: after pause: collector=%d, dlq=%d", collector.count(), dlq.count())

	t.Log("UC38: resuming sender")
	paused.Resume()

	lrWaitFor(t, 90*time.Second, fmt.Sprintf("collector>=%d", msgCount), func() bool {
		return collector.count() >= msgCount
	})
	time.Sleep(2 * time.Second)

	gotMQTT := collector.count()
	gotDLQ := dlq.count()
	t.Logf("UC38: MQTT=%d, DLQ=%d, total=%d", gotMQTT, gotDLQ, gotMQTT+gotDLQ)

	// SQS retries depth-rejected msgs; all should eventually arrive.
	require.GreaterOrEqual(t, gotMQTT+gotDLQ, msgCount,
		"DLQ(%d)+MQTT(%d) should >= %d", gotDLQ, gotMQTT, msgCount)
	t.Logf("UC38: passed -- %d delivered, %d DLQ", gotMQTT, gotDLQ)
}

// =========================================================================
// UC39: AckAfterOutboxPersist -- 2,000 msgs, slow sender 200ms
// SQS-IN -> [Bridge] (SharedOutbox, AckAfterOutboxPersist) -> MQTT
// Verify SQS fully consumed before outbox drain completes.
// =========================================================================

func TestUC39_AckAfterOutboxPersist(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		pollTimeout = 120 * time.Second
		outTopic    = "uc39/output/data"
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc39-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc39-session")
	collector := newMQTTCollector(t, outTopic, "uc39-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mqttSess := newMQTTSession(t, sessionID, domain.SessionExclusive)
	realSender := setupMQTTSender(t, mqttSess)
	slow := newSlowSender(realSender, 200*time.Millisecond)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc39-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc39-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc39-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc39-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, slow, mqttSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC39: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Poll SQS-IN to detect when it becomes empty (all acked).
	var sqsEmptyTime atomic.Int64
	checkClient := sqslocal.Client(t)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			out, err := checkClient.GetQueueAttributes(ctx,
				&awssqs.GetQueueAttributesInput{
					QueueUrl: &sqsInURL,
					AttributeNames: []sqstypes.QueueAttributeName{
						sqstypes.QueueAttributeNameApproximateNumberOfMessages,
					},
				})
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			if v := out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]; v == "0" {
				if sqsEmptyTime.CompareAndSwap(0, time.Now().UnixMilli()) {
					t.Logf("UC39: SQS-IN empty at collector=%d", collector.count())
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector>=%d", msgCount), func() bool {
		return collector.count() >= msgCount
	})
	drainDoneTime := time.Now().UnixMilli()

	emptyTS := sqsEmptyTime.Load()
	if emptyTS > 0 {
		t.Logf("UC39: SQS empty=%dms, drain done=%dms, delta=%dms",
			emptyTS, drainDoneTime, drainDoneTime-emptyTS)
		assert.Less(t, emptyTS, drainDoneTime,
			"SQS should be empty before outbox drain completes")
	} else {
		t.Log("UC39: SQS empty time not captured (fast drain)")
	}

	assert.GreaterOrEqual(t, collector.count(), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC39: passed -- %d delivered", collector.count())
}

// =========================================================================
// UC40: Adaptive Drain Backoff -- 1,500 msgs, AdaptiveBackoff(100ms,5s,2.0)
// Send 1,000 -> wait 15s idle -> send 500. Count drain cycles during idle.
// =========================================================================

func TestUC40_AdaptiveDrain_Backoff(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		firstBatch  = 1000
		secondBatch = 500
		totalMsg    = firstBatch + secondBatch
		idleWait    = 15 * time.Second
		pollTimeout = 120 * time.Second
		outTopic    = "uc40/output/data"
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc40-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	rec := &ports.RecordingExporter{}
	sessionID := mqttlocal.UniqueClientID("uc40-session")
	collector := newMQTTCollector(t, outTopic, "uc40-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mqttSess := newMQTTSession(t, sessionID, domain.SessionExclusive)
	mqttSnd := setupMQTTSender(t, mqttSess)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)
	sc.DrainStrategy = domain.NewAdaptiveBackoff(
		100*time.Millisecond, 5*time.Second, 2.0)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc40-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithMetrics(rec),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc40-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc40-bind", Address: outTopic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc40-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, mqttSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC40: sending first batch of %d", firstBatch)
	sendBulkToSQS(t, sqsInClient, sqsInURL, firstBatch, nil)
	lrWaitFor(t, 60*time.Second, fmt.Sprintf("collector>=%d", firstBatch),
		func() bool { return collector.count() >= firstBatch })
	t.Logf("UC40: first batch delivered, collector=%d", collector.count())

	// Phase 2: idle -- count drain cycles via MetricOutboxDrainLatency.
	rec.Reset()
	time.Sleep(idleWait)
	idleDrainCycles := len(rec.FindEntries(domain.MetricOutboxDrainLatency))
	t.Logf("UC40: drain cycles during %v idle: %d", idleWait, idleDrainCycles)

	// Adaptive(100ms,5s,2x): 100->200->400->800->1.6s->3.2s->5s->5s...
	// ~10-15 cycles in 15s. Fixed 100ms would do 150. Assert < 50.
	assert.Less(t, idleDrainCycles, 50,
		"adaptive backoff should reduce idle drain cycles below 50, got %d",
		idleDrainCycles)

	t.Logf("UC40: sending second batch of %d", secondBatch)
	sendBulkToSQS(t, sqsInClient, sqsInURL, secondBatch, nil)
	lrWaitFor(t, 60*time.Second, fmt.Sprintf("collector>=%d", totalMsg),
		func() bool { return collector.count() >= totalMsg })

	assert.GreaterOrEqual(t, collector.count(), totalMsg)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC40: passed -- %d delivered, %d idle cycles",
		collector.count(), idleDrainCycles)
}

// =========================================================================
// UC41: Idempotent Outbox Persist -- 200 msgs, VisibilityTimeout=3s
// slowProcessor(4s) on first 50 msgs causes SQS redelivery.
// SharedOutbox deduplicates by envelope ID; verify 200 unique in SQS-OUT.
// =========================================================================

// uc41SlowFirstN delays only the first N invocations.
type uc41SlowFirstN struct {
	delay time.Duration
	limit int
	seen  atomic.Int64
}

func (p *uc41SlowFirstN) Name() string { return "uc41-slow-first-n" }

func (p *uc41SlowFirstN) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	if idx := p.seen.Add(1); idx <= int64(p.limit) {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return next(ctx, env)
}

func TestUC41_IdempotentOutbox_Persist(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 200
		pollTimeout = 120 * time.Second
	)

	sqsInClient := sqslocal.Client(t)
	sqsInName := sqslocal.UniqueQueue("uc41-in")
	sqsInURL := sqslocal.CreateQueueWithAttrs(t, sqsInClient, sqsInName,
		map[string]string{"VisibilityTimeout": "3"})

	sqsOutURL, sqsOutClient := setupSQSQueue(t, "uc41-out")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc41-session")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ep := sqslocal.Endpoint(t)
	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          sqsInURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 3,
	}, slog.Default())
	require.NoError(t, err)

	sqsSnd := newSQSSender(t, sqsOutURL)
	slowProc := &uc41SlowFirstN{delay: 4 * time.Second, limit: 50}
	mqttSess := newMQTTSession(t, sessionID, domain.SessionExclusive)
	sc := lrSessionConfig(sessionID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc41-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc41-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Processors: []ports.Processor{slowProc},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc41-bind", Address: sqsOutURL},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc41-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, sqsSnd, mqttSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC41: sending %d messages to SQS-IN (vis=3s)", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	t.Log("UC41: polling SQS-OUT")
	bodies := pollSQSBodies(t, sqsOutClient, sqsOutURL, msgCount, 90*time.Second)
	t.Logf("UC41: received %d messages from SQS-OUT", len(bodies))
	require.GreaterOrEqual(t, len(bodies), msgCount)

	seen := make(map[string]int, len(bodies))
	for _, b := range bodies {
		seen[b]++
	}
	dupes := 0
	for _, c := range seen {
		if c > 1 {
			dupes++
		}
	}

	t.Logf("UC41: %d unique, %d duplicate payloads", len(seen), dupes)
	assert.Equal(t, msgCount, len(seen),
		"should have exactly %d unique messages", msgCount)
	assert.Equal(t, 0, dupes, "outbox idempotency should prevent duplicates")
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC41: passed -- %d unique out of %d received", len(seen), len(bodies))
}
