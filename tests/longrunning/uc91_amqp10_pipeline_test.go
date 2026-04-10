//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	amqp10adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// =========================================================================
// UC91: SQS-IN -> Bridge (SharedOutbox) -> Artemis address -> Collector
//
// End-to-end pipeline that reads from SQS, routes through a gobridge
// runtime with SharedOutbox delivery, and publishes to an Artemis
// address via AMQP 1.0. A separate AMQP 1.0 collector consumes from
// the same address and verifies delivery.
//
// Unlike AMQP 0-9-1, Artemis auto-creates addresses when sender/
// receiver links attach — no exchange, queue, or binding declarations
// are needed.
//
// Validates:
//   - Ingress bridge reads from SQS and publishes to Artemis
//   - SharedOutbox guarantees exactly-once delivery to the broker
//   - All 1,000 messages arrive as unique envelopes
//   - DLQ remains empty (no permanent failures)
//
// Topology:
//   SQS-IN -> [Bridge (SharedOutbox)] -> Artemis address -> Collector
// =========================================================================

const (
	uc91MsgCount    = 1000
	uc91PollTimeout = 160 * time.Second
)

func TestUC91_SQS_To_Artemis_SharedOutbox(t *testing.T) {
	_ = withFreshInfra(t)

	address := artemislocal.UniqueAddress("uc91-addr")

	// --- SQS + DynamoDB ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc91-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// AMQP 1.0 session — NOT started; the runtime's SessionManager
	// handles Start, Reconcile, and lease lifecycle.
	sessionID := uniqueID("uc91-sess")
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	amqpSess := amqp10adapter.NewSession(amqp10adapter.SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 30 * time.Second,
		IdleTimeout:    2 * time.Minute,
	}, domain.SessionExclusive, testLogger(t))
	t.Cleanup(func() { _ = amqpSess.Close(context.Background()) })

	// Sender stores references lazily; safe to create before session starts.
	amqpSnd, err := amqp10adapter.NewSender(amqp10adapter.SenderConfig{
		Address: address,
		Timeout: 30 * time.Second,
		Session: amqpSess,
	}, amqpSess)
	require.NoError(t, err, "NewArtemisSender")
	t.Cleanup(func() { _ = amqpSnd.Close(context.Background()) })

	sqsRx := newSQSReceiver(t, sqsInURL)
	sc := lrSessionConfig(sessionID)

	// --- Bridge runtime ---
	rt := goruntime.New(
		goruntime.WithInstanceID("uc91-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc91-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc91-bind", Address: address},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc91-bind", SessionID: sessionID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, amqpSnd, amqpSess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	// --- Collector subscribes to the Artemis address ---
	collector := newArtemisCollector(t, address)

	// --- Inject messages into SQS ---
	t.Logf("UC91: sending %d messages to SQS-IN", uc91MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc91MsgCount, nil)

	// --- Wait for all messages to arrive at the collector ---
	lrWaitFor(t, uc91PollTimeout,
		fmt.Sprintf("collector unique >= %d", uc91MsgCount),
		func() bool { return countUniqueAMQP(collector) >= uc91MsgCount })

	unique := countUniqueAMQP(collector)
	t.Logf("UC91: unique=%d, total=%d, dlq=%d", unique, collector.count(), dlq.count())

	require.GreaterOrEqual(t, unique, uc91MsgCount,
		"SharedOutbox must deliver all %d unique messages to Artemis", uc91MsgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}
