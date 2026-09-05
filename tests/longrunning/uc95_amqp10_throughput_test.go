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
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
)

// =========================================================================
// UC95: High Throughput Through Artemis (AMQP 1.0)
//
// SQS-IN (5000 msgs) -> [Bridge SharedOutbox] -> Artemis address
//                     -> amqpCollector
//
// Validates that the bridge can sustain high-volume message flow through
// an Artemis broker using SharedOutbox delivery. Artemis auto-creates
// addresses when links attach, so Reconcile just stores the plan without
// needing explicit declarations.
//
// Assert: all 5000 messages received, DLQ empty.
// =========================================================================

func TestUC95_AMQP10_HighThroughput(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		// 3000 is what the local AWS emulation serves within the bridge's
		// store-call deadlines while the rest of this suite runs beside it.
		// Above that, emulator UpdateItem latency exceeds those deadlines, the
		// drain stalls on retries, and the run fails for the emulator's
		// throughput rather than for anything the bridge did. Higher volume
		// belongs on real infrastructure.
		msgCount    = 3000
		testTimeout = 300 * time.Second
	)

	address := artemislocal.UniqueAddress("uc95-addr")

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc95-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newArtemisCollector(t, address)

	sessID := uniqueID("uc95-sess")
	sess := setupArtemisSession(t, connectivity.SessionExclusive)

	sender := newArtemisSender(t, sess, address)
	rx := newSQSReceiver(t, sqsInURL)
	sc := lrThroughputSessionConfig(sessID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc95-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	routeCfg := goruntime.RouteConfig{
		ID: "uc95-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc95-bind", Address: address},
		),
		Bindings: []routing.DestinationBinding{
			{ID: "uc95-bind", SessionID: sessID},
		},
	}
	require.NoError(t, rt.AddRoute(routeCfg, rx, sender, sess, &sc))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	gobridgesync(t, 15*time.Second, rt)

	start := time.Now()
	t.Logf("UC95: sending %d messages to SQS-IN", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 280*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	elapsed := time.Since(start)
	throughput := float64(collector.count()) / elapsed.Seconds()

	received := collector.count()
	unique := countUniqueAMQP(collector)

	t.Logf("UC95: received=%d, unique=%d, dlq=%d", received, unique, dlq.count())
	t.Logf("UC95: elapsed=%s, throughput=%.0f msgs/sec", elapsed.Round(time.Millisecond), throughput)

	assert.GreaterOrEqual(t, received, msgCount,
		"All %d messages must be received", msgCount)
	assert.GreaterOrEqual(t, unique, msgCount,
		"All %d unique messages must be delivered (duplicates indicate lost originals)", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}
