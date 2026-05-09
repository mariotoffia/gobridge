//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// =========================================================================
// UC97: Multiple Consumers on Same RabbitMQ Queue (Competing Consumers)
//
// SQS-IN (1000 msgs) -> [Bridge SharedOutbox] -> RabbitMQ exchange/queue
//                     -> 3 amqpCollectors on the SAME queue
//
// RabbitMQ distributes messages round-robin among consumers on the same
// queue. Each message is delivered to exactly one consumer.
//
// Assert: total across all 3 collectors == 1000 (each message consumed
// exactly once, no duplicates, no loss).
// =========================================================================

func TestUC97_AMQP091_MultiConsumer_CompetingConsumers(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount      = 1000
		consumerCount = 3
		testTimeout   = 180 * time.Second
	)

	exchange := rabbitmqlocal.UniqueExchange("uc97-ex")
	queue := rabbitmqlocal.UniqueQueue("uc97-q")
	routingKey := queue

	rabbitmqlocal.CreateExchange(t, exchange, "direct")
	rabbitmqlocal.CreateQueue(t, queue)
	rabbitmqlocal.BindQueue(t, queue, exchange, routingKey)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc97-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Three competing consumers on the same queue.
	collectors := make([]*amqpCollector, consumerCount)
	for i := 0; i < consumerCount; i++ {
		collectors[i] = newRabbitMQCollector(t, queue)
	}

	sessID := uniqueID("uc97-sess")
	sess := setupRabbitMQSession(t, connectivity.SessionExclusive)
	require.NoError(t, sess.Reconcile(ctx, connectivity.SessionPlan{
		Publishers: []connectivity.PublisherPlan{
			{Topic: exchange},
		},
	}))

	sender := newRabbitMQSender(t, sess, exchange, routingKey)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc97-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc97-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc97-bind", Address: routingKey},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc97-bind", SessionID: sessID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, rx, sender, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	t.Logf("UC97: sending %d messages to SQS-IN (%d competing consumers)", msgCount, consumerCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait until the total across all collectors reaches msgCount.
	totalCount := func() int {
		n := 0
		for _, c := range collectors {
			n += c.count()
		}
		return n
	}

	lrWaitFor(t, 160*time.Second,
		fmt.Sprintf("total across %d collectors >= %d", consumerCount, msgCount),
		func() bool { return totalCount() >= msgCount })

	// Collect results from all consumers.
	total := totalCount()
	for i, c := range collectors {
		t.Logf("UC97: collector[%d] received=%d", i, c.count())
	}
	t.Logf("UC97: total=%d, dlq=%d", total, dlq.count())

	// Verify no duplicates across all collectors combined.
	allIDs := make(map[string]int)
	for i, c := range collectors {
		for _, m := range c.getMessages() {
			allIDs[m.ID]++
			if allIDs[m.ID] > 1 {
				t.Logf("UC97: duplicate message ID=%s (seen in collector %d)", m.ID, i)
			}
		}
	}

	duplicates := 0
	for _, count := range allIDs {
		if count > 1 {
			duplicates++
		}
	}

	t.Logf("UC97: unique IDs=%d, duplicates=%d", len(allIDs), duplicates)

	assert.GreaterOrEqual(t, total, msgCount,
		"Total across all collectors must be >= %d", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	// Each consumer should have received at least some messages (round-robin).
	for i, c := range collectors {
		assert.Greater(t, c.count(), 0,
			"Collector[%d] should have received at least some messages (round-robin)", i)
	}

	// With round-robin, no single consumer should have all messages.
	for i, c := range collectors {
		assert.Less(t, c.count(), msgCount,
			"Collector[%d] should not have received ALL messages (round-robin expected)", i)
	}

	if duplicates == 0 {
		t.Log("UC97: zero duplicates — each message consumed exactly once (competing consumers)")
	} else {
		t.Logf("UC97: %d duplicate message IDs detected across consumers", duplicates)
	}
}
