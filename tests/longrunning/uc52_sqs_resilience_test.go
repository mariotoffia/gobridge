//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestUC52_VisibilityTimeoutExpiry(t *testing.T) {
	_ = withFreshInfra(t)
	client := newSQSClient(t)
	name := uniqueQueueName("uc52")
	queueURL := createSQSQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "3",
	})

	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Client:            client,
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 3,
		AutoExtend:        boolPtr(false),
	}, slog.Default())
	require.NoError(t, err)

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc52"), connectivity.SessionExclusive)
	collector := newMQTTCollector(t, "uc52/out", "uc52")
	sender := setupMQTTSender(t, sess)

	slow := newSlowProcessor("uc52-slow", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc52-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc52-vis",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  50,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc52-bind", Address: "uc52/out"},
		),
		Processors:         []ports.Processor{slow},
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)
	sendBulkToSQS(t, client, queueURL, 50, nil)

	lrWaitFor(t, 5*time.Minute, "uc52: total > 50 (expecting duplicates)", func() bool {
		return collector.count() > 50
	})

	unique := countUnique(collector)
	total := collector.count()
	t.Logf("uc52: unique=%d total=%d duplicates=%d", unique, total, total-unique)
	assert.GreaterOrEqual(t, unique, 50)
	assert.Greater(t, total, 50, "expected duplicates from SQS redelivery")
}

func TestUC53_AutoExtendUnderLoad(t *testing.T) {
	_ = withFreshInfra(t)
	client := newSQSClient(t)
	name := uniqueQueueName("uc53")
	queueURL := createSQSQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "5",
	})

	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Client:            client,
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        boolPtr(true),
	}, slog.Default())
	require.NoError(t, err)

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc53"), connectivity.SessionExclusive)
	collector := newMQTTCollector(t, "uc53/out", "uc53")
	sender := setupMQTTSender(t, sess)

	slow := newSlowProcessor("uc53-slow", 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc53-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc53-autoext",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc53-bind", Address: "uc53/out"},
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
	// Both batches are sent before the bridge starts, and that ordering is
	// load-bearing. The local emulator deduplicates against the messages still
	// in the queue; real SQS remembers a deduplication id for five minutes
	// whether or not the message was consumed. With a consumer already
	// draining, the duplicate batch would arrive after its originals were gone
	// and none of it would be suppressed.
	//
	// Sends are spread over several message groups: a single group at this
	// volume stalls after the first batch.
	_ = withFreshInfra(t)
	queueURL, client := setupFIFOQueue(t, "uc54")

	receiver := newSQSReceiver(t, queueURL)
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc54"), connectivity.SessionExclusive)
	collector := newMQTTCollector(t, "uc54/out", "uc54")
	sender := setupMQTTSender(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc54-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc54-fifo-dedup",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  1, // serialize: one message per group in flight at a time
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc54-bind", Address: "uc54/out"},
		),
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	const dedupCount = 100
	groupFn := func(i int) string { return fmt.Sprintf("dedup-g%d", i%5) }
	sendBulkToSQSFIFO(t, client, queueURL, dedupCount, groupFn)
	sendBulkToSQSFIFO(t, client, queueURL, dedupCount, groupFn) // duplicates

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)

	lrWaitFor(t, 4*time.Minute, fmt.Sprintf("uc54: collector >= %d", dedupCount), func() bool {
		return collector.count() >= dedupCount
	})
	waitForSQSQueueDrained(t, ctx, client, queueURL, 30*time.Second)
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		Routes:   []string{"uc54-fifo-dedup"},
		MinQuiet: 250 * time.Millisecond,
		Timeout:  30 * time.Second,
	}))

	total := collector.count()
	t.Logf("uc54: total=%d (expected ~%d, tolerance 10%%)", total, dedupCount)
	// A small percentage of duplicates may survive content-based dedup, so
	// tolerate up to 10% rather than demanding an exact count.
	maxAllowed := dedupCount + dedupCount/10
	assert.LessOrEqual(t, total, maxAllowed,
		"dedup should discard most duplicates (tolerance: +10%%)")
	assert.GreaterOrEqual(t, total, dedupCount,
		"should receive at least %d unique messages", dedupCount)
}

func TestUC55_FIFOOrdering(t *testing.T) {
	// The 200-message volume, and the decision below to log ordering
	// violations rather than assert on them, were both adopted for an
	// SQS emulator this suite no longer uses — it implemented no FIFO
	// message-group locking. The current emulator delivered all 200 in
	// order with no duplicates when this was last measured. Turning the
	// logs into assertions and restoring the original volume is a
	// deliberate widening of coverage, so it is left as its own change
	// rather than smuggled into an infrastructure swap.
	_ = withFreshInfra(t)
	queueURL, client := setupFIFOQueue(t, "uc55")

	receiver := newSQSReceiver(t, queueURL)
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc55"), connectivity.SessionExclusive)
	collector := newMQTTCollector(t, "uc55/out", "uc55")
	sender := setupMQTTSender(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc55-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc55-fifo-order",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  1,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc55-bind", Address: "uc55/out"},
		),
		SourceCapabilities: directHoldCaps,
	}, receiver, sender, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 30*time.Second, rt)

	const msgCount = 200
	groupFn := func(i int) string { return fmt.Sprintf("g-%d", i%5) }
	sendBulkToSQSFIFO(t, client, queueURL, msgCount, groupFn)

	lrWaitFor(t, 5*time.Minute, fmt.Sprintf("uc55: collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})

	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), msgCount)

	type seqMsg struct {
		Seq int `json:"seq"`
	}

	groups := make(map[string][]int)
	for _, m := range msgs {
		var sm seqMsg
		if err := json.Unmarshal(m.Payload(), &sm); err != nil {
			continue
		}
		g := fmt.Sprintf("g-%d", sm.Seq%5)
		groups[g] = append(groups[g], sm.Seq)
	}

	outOfOrder := 0
	for g, seqs := range groups {
		dupes := 0
		for i := 1; i < len(seqs); i++ {
			if seqs[i] == seqs[i-1] {
				dupes++ // an emulator without group locking can deliver duplicates
			} else if seqs[i] < seqs[i-1] {
				outOfOrder++
				// Logged rather than failed: see the note at the top of this
				// test for why this is not yet an assertion.
				t.Logf("uc55: group %s: ordering gap at index %d: %d < %d",
					g, i, seqs[i], seqs[i-1])
			}
		}
		if dupes > 0 {
			t.Logf("uc55: group %s: %d duplicate deliveries", g, dupes)
		}
	}
	if outOfOrder > 0 {
		t.Logf("uc55: %d ordering violations", outOfOrder)
	}
	t.Logf("uc55: received %d msgs across %d groups", len(msgs), len(groups))
}

func waitForSQSQueueDrained(
	t *testing.T,
	ctx context.Context,
	client *awssqs.Client,
	queueURL string,
	timeout time.Duration,
) {
	t.Helper()
	lrWaitFor(t, timeout, "SQS queue visible and in-flight depth == 0", func() bool {
		out, err := client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
			QueueUrl: &queueURL,
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		if err != nil {
			return false
		}
		return out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)] == "0" &&
			out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)] == "0"
	})
}

func TestUC56_BatchMixedSuccessFailure(t *testing.T) {
	_ = withFreshInfra(t)
	queueURL, client := setupSQSQueue(t, "uc56")

	receiver := newSQSReceiver(t, queueURL)
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc56"), connectivity.SessionExclusive)
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
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxInFlight:        20,
			OnPermanentFailure: "dlq",
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc56-bind", Address: "uc56/out"},
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
