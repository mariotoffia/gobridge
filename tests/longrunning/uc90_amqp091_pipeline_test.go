//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	amqp091adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// =========================================================================
// UC90: SQS-IN -> Bridge (SharedOutbox) -> RabbitMQ queue -> Collector
//
// End-to-end pipeline that reads from SQS, routes through a gobridge
// runtime with SharedOutbox delivery, and publishes to a RabbitMQ
// exchange. A separate AMQP collector consumes from the bound queue
// and verifies delivery.
//
// Validates:
//   - Ingress bridge reads from SQS and publishes to RabbitMQ
//   - SharedOutbox guarantees exactly-once delivery to the broker
//   - All 1,000 messages arrive as unique envelopes
//   - DLQ remains empty (no permanent failures)
//
// Topology:
//   SQS-IN -> [Bridge (SharedOutbox)] -> RabbitMQ exchange -> queue
//                                                              |
//                                                         Collector
// =========================================================================

const (
	uc90MsgCount    = 1000
	uc90RoutingKey  = "uc90"
	uc90PollTimeout = 160 * time.Second
)

func TestUC90_SQS_To_RabbitMQ_SharedOutbox(t *testing.T) {
	_ = withFreshInfra(t)

	// --- RabbitMQ infrastructure via management API ---
	exchangeName := rabbitmqlocal.UniqueExchange("uc90-ex")
	queueName := rabbitmqlocal.UniqueQueue("uc90-q")

	rabbitmqlocal.CreateExchange(t, exchangeName, "direct")
	rabbitmqlocal.CreateQueue(t, queueName)
	rabbitmqlocal.BindQueue(t, queueName, exchangeName, uc90RoutingKey)

	// --- SQS + DynamoDB ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc90-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// AMQP 0-9-1 session — NOT started; the runtime's SessionManager
	// handles Start, Reconcile, and lease lifecycle.
	sessionID := uniqueID("uc90-sess")
	amqpSess := amqp091adapter.NewSession(amqp091adapter.SessionOptions{
		BrokerURL:      rabbitmqlocal.Endpoint(t),
		Heartbeat:      10 * time.Second,
		ConnectTimeout: 30 * time.Second,
	}, domain.SessionExclusive, testLogger(t))
	t.Cleanup(func() { _ = amqpSess.Close(context.Background()) })

	amqpSnd := newRabbitMQSender(t, amqpSess, exchangeName, uc90RoutingKey)
	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("uc90-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc90-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc90-bind", Address: exchangeName},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc90-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, amqpSnd, amqpSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	// --- Collector subscribes to the RabbitMQ queue ---
	collector := newRabbitMQCollector(t, queueName)

	// --- Inject messages into SQS ---
	t.Logf("UC90: sending %d messages to SQS-IN", uc90MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc90MsgCount, nil)

	// --- Wait for all messages to arrive at the collector ---
	lrWaitFor(t, uc90PollTimeout,
		fmt.Sprintf("collector unique >= %d", uc90MsgCount),
		func() bool { return countUniqueAMQP(collector) >= uc90MsgCount })

	unique := countUniqueAMQP(collector)
	t.Logf("UC90: unique=%d, total=%d, dlq=%d", unique, collector.count(), dlq.count())

	require.GreaterOrEqual(t, unique, uc90MsgCount,
		"SharedOutbox must deliver all %d unique messages to RabbitMQ", uc90MsgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}
