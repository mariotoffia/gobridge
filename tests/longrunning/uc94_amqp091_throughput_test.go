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
// UC94: High Throughput Through RabbitMQ (AMQP 0-9-1)
//
// SQS-IN (5000 msgs) -> [Bridge SharedOutbox] -> RabbitMQ exchange/queue
//                     -> amqpCollector
//
// Validates that the bridge can sustain high-volume message flow through
// a RabbitMQ broker using SharedOutbox delivery. Each test run uses a
// unique exchange and queue to prevent cross-test interference.
//
// Assert: all 5000 messages received, DLQ empty.
// =========================================================================

func TestUC94_AMQP091_HighThroughput(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 5000
		testTimeout = 300 * time.Second
	)

	exchange := rabbitmqlocal.UniqueExchange("uc94-ex")
	queue := rabbitmqlocal.UniqueQueue("uc94-q")
	routingKey := queue

	rabbitmqlocal.CreateExchange(t, exchange, "direct")
	rabbitmqlocal.CreateQueue(t, queue)
	rabbitmqlocal.BindQueue(t, queue, exchange, routingKey)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc94-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newRabbitMQCollector(t, queue)

	sessID := uniqueID("uc94-sess")
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
		goruntime.WithInstanceID("uc94-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc94-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc94-bind", Address: exchange},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc94-bind", SessionID: sessID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, rx, sender, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	start := time.Now()
	t.Logf("UC94: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 280*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	elapsed := time.Since(start)
	throughput := float64(collector.count()) / elapsed.Seconds()

	received := collector.count()
	unique := countUniqueAMQP(collector)

	t.Logf("UC94: received=%d, unique=%d, dlq=%d", received, unique, dlq.count())
	t.Logf("UC94: elapsed=%s, throughput=%.0f msgs/sec", elapsed.Round(time.Millisecond), throughput)

	assert.GreaterOrEqual(t, received, msgCount,
		"All %d messages must be received", msgCount)
	assert.GreaterOrEqual(t, unique, msgCount,
		"All %d unique messages must be delivered (duplicates indicate lost originals)", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}
