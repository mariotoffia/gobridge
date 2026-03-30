//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

func TestUC52_VisibilityTimeoutExpiry(t *testing.T) {
	_ = withFreshInfra(t)
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("uc52")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "3",
	})

	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          sqslocal.Endpoint(t),
		Region:            "us-east-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 3,
		AutoExtend:        boolPtr(false),
	}, slog.Default())
	require.NoError(t, err)

	sess := newMQTTSession(t, mqttlocal.UniqueClientID("uc52"), domain.SessionExclusive)
	collector := newMQTTCollector(t, "uc52/out", "uc52")
	sender := setupMQTTSender(t, sess)

	slow := newSlowProcessor("uc52-slow", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc52-bridge"),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc52-vis",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  5,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc52-bind", Address: "uc52/out"},
		),
		Processors:         []ports.Processor{slow},
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)
	sendBulkToSQS(t, client, queueURL, 50, nil)

	lrWaitFor(t, 5*time.Minute, "uc52: unique >= 50", func() bool {
		return countUnique(collector) >= 50
	})

	unique := countUnique(collector)
	total := collector.count()
	t.Logf("uc52: unique=%d total=%d duplicates=%d", unique, total, total-unique)
	assert.GreaterOrEqual(t, unique, 50)
	assert.Greater(t, total, 50, "expected duplicates from SQS redelivery")
}

func TestUC53_AutoExtendUnderLoad(t *testing.T) {
	_ = withFreshInfra(t)
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("uc53")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "5",
	})

	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          sqslocal.Endpoint(t),
		Region:            "us-east-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        boolPtr(true),
	}, slog.Default())
	require.NoError(t, err)

	sess := newMQTTSession(t, mqttlocal.UniqueClientID("uc53"), domain.SessionExclusive)
	collector := newMQTTCollector(t, "uc53/out", "uc53")
	sender := setupMQTTSender(t, sess)

	slow := newSlowProcessor("uc53-slow", 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc53-bridge"),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc53-autoext",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc53-bind", Address: "uc53/out"},
		),
		Processors:         []ports.Processor{slow},
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)
	sendBulkToSQS(t, client, queueURL, 200, nil)

	lrWaitFor(t, 15*time.Minute, "uc53: unique >= 200", func() bool {
		return countUnique(collector) >= 200
	})

	unique := countUnique(collector)
	total := collector.count()
	t.Logf("uc53: unique=%d total=%d duplicates=%d", unique, total, total-unique)
	assert.GreaterOrEqual(t, unique, 200)
}

func TestUC54_FIFODeduplication(t *testing.T) {
	_ = withFreshInfra(t)
	queueURL, client := setupFIFOQueue(t, "uc54")

	receiver := newSQSReceiver(t, queueURL)
	sess := newMQTTSession(t, mqttlocal.UniqueClientID("uc54"), domain.SessionExclusive)
	collector := newMQTTCollector(t, "uc54/out", "uc54")
	sender := setupMQTTSender(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc54-bridge"),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc54-fifo-dedup",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  10,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc54-bind", Address: "uc54/out"},
		),
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)

	groupFn := func(i int) string { return "dedup-group" }
	sendBulkToSQSFIFO(t, client, queueURL, 500, groupFn)
	sendBulkToSQSFIFO(t, client, queueURL, 500, groupFn)

	lrWaitFor(t, 3*time.Minute, "uc54: collector >= 500", func() bool {
		return collector.count() >= 500
	})
	time.Sleep(10 * time.Second)

	total := collector.count()
	t.Logf("uc54: total=%d (expected exactly 500)", total)
	assert.Equal(t, 500, total, "FIFO content-based dedup should discard duplicates")
}

func TestUC55_FIFOOrdering(t *testing.T) {
	_ = withFreshInfra(t)
	queueURL, client := setupFIFOQueue(t, "uc55")

	receiver := newSQSReceiver(t, queueURL)
	sess := newMQTTSession(t, mqttlocal.UniqueClientID("uc55"), domain.SessionExclusive)
	collector := newMQTTCollector(t, "uc55/out", "uc55")
	sender := setupMQTTSender(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc55-bridge"),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc55-fifo-order",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  1,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc55-bind", Address: "uc55/out"},
		),
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)

	groupFn := func(i int) string { return fmt.Sprintf("g-%d", i%5) }
	sendBulkToSQSFIFO(t, client, queueURL, 1000, groupFn)

	lrWaitFor(t, 5*time.Minute, "uc55: collector >= 1000", func() bool {
		return collector.count() >= 1000
	})

	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), 1000)

	type seqMsg struct {
		Seq int `json:"seq"`
	}

	groups := make(map[string][]int)
	for _, m := range msgs {
		var sm seqMsg
		if err := json.Unmarshal(m.Payload, &sm); err != nil {
			continue
		}
		g := fmt.Sprintf("g-%d", sm.Seq%5)
		groups[g] = append(groups[g], sm.Seq)
	}

	for g, seqs := range groups {
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1] {
				t.Errorf("group %s: ordering violated at index %d: %d <= %d",
					g, i, seqs[i], seqs[i-1])
				break
			}
		}
	}
	t.Logf("uc55: received %d msgs across %d groups", len(msgs), len(groups))
}

func TestUC56_BatchMixedSuccessFailure(t *testing.T) {
	_ = withFreshInfra(t)
	queueURL, client := setupSQSQueue(t, "uc56")

	receiver := newSQSReceiver(t, queueURL)
	sess := newMQTTSession(t, mqttlocal.UniqueClientID("uc56"), domain.SessionExclusive)
	collector := newMQTTCollector(t, "uc56/out", "uc56")
	sender := setupMQTTSender(t, sess)

	dlq := &lrDLQStore{}
	rejector := &rejectEveryNthProcessor{n: 5}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc56-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc56-mixed",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        20,
			OnPermanentFailure: "dlq",
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc56-bind", Address: "uc56/out"},
		),
		Processors:         []ports.Processor{rejector},
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)
	sendBulkToSQS(t, client, queueURL, 1000, nil)

	lrWaitFor(t, 5*time.Minute, "uc56: collector+dlq >= 1000", func() bool {
		return collector.count()+dlq.count() >= 1000
	})

	delivered := collector.count()
	rejected := dlq.count()
	t.Logf("uc56: delivered=%d rejected=%d total=%d", delivered, rejected, delivered+rejected)
	assert.Equal(t, 800, delivered, "expected 800 delivered (4/5 of 1000)")
	assert.Equal(t, 200, rejected, "expected 200 rejected (1/5 of 1000)")
}
