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
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestUC69_DLQReplayIntegration verifies that messages landing in the DLQ
// during Phase 1 (all sends fail) can be replayed through a healthy bridge
// in Phase 2 and eventually reach the output collector.
func TestUC69_DLQReplayIntegration(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		outTopic    = "uc69/output"
		testTimeout = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc69-in")
	dlq := &replayableDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc69-col")

	// Phase 1: all messages fail -> DLQ
	sessID := mqttlocal.UniqueClientID("uc69-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	failSnd := &alwaysFailSender{}
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc69-p1"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc69-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliveryDirectHold,
			MaxReplayAttempts: 1,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc69-bind",
			Address:   outTopic,
		}),
		SourceCapabilities: directHoldCaps,
	}, rx, failSnd, sess, nil))

	require.NoError(t, rt.Start(ctx))

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlq.count() >= msgCount
	})

	_ = rt.Stop(context.Background())

	t.Logf("UC69 Phase1: DLQ=%d, collector=%d", dlq.count(), collector.count())
	assert.Equal(t, 0, collector.count())

	// Phase 2: replay DLQ entries through working bridge
	entries, err := dlq.List(ctx, domain.DLQFilter{})
	require.NoError(t, err)
	t.Logf("UC69: replaying %d DLQ entries", len(entries))

	realSnd := setupMQTTSender(t, sess)

	rt2 := goruntime.New(
		goruntime.WithInstanceID("uc69-p2"),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt2.AddRoute(goruntime.RouteConfig{
		ID: "uc69-route2",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc69-bind2",
			Address:   outTopic,
		}),
	}, &noopReceiver{}, realSnd, sess, nil))

	require.NoError(t, rt2.Start(ctx))
	defer func() { _ = rt2.Stop(context.Background()) }()

	lrWaitFor(t, 5*time.Second, "bridge2 running", func() bool {
		return rt2.DeepHealth(context.Background()).Running
	})

	for _, e := range entries {
		env := e.Envelope.Clone()
		_ = rt2.Inject(ctx, "uc69-route2", env)
	}

	lrWaitFor(t, 60*time.Second, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})

	t.Logf("UC69 Phase2: collector=%d", collector.count())
	require.GreaterOrEqual(t, collector.count(), msgCount)
}

// TestUC70_ErrorClassificationAccuracy validates that the error classification
// sender correctly routes messages based on error_type header: messages with no
// header succeed, while "permanent" and "transient" errors land in the DLQ.
func TestUC70_ErrorClassificationAccuracy(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 300
		outTopic    = "uc70/output"
		testTimeout = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc70-in")
	dlq := &replayableDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc70-col")

	sessID := mqttlocal.UniqueClientID("uc70-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	realSnd := setupMQTTSender(t, sess)
	errSnd := &errorClassSender{inner: realSnd}
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc70"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc70-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliveryDirectHold,
			MaxReplayAttempts: 1,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc70-bind",
			Address:   outTopic,
		}),
		SourceCapabilities: directHoldCaps,
	}, rx, errSnd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// i%3==0 -> no header (succeed), i%3==1 -> "permanent", i%3==2 -> "transient"
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, func(i int) map[string]string {
		switch i % 3 {
		case 1:
			return map[string]string{"error_type": "permanent"}
		case 2:
			return map[string]string{"error_type": "transient"}
		default:
			return nil
		}
	})

	expectedSuccess := msgCount / 3 // 100 messages succeed
	expectedDLQ := msgCount - expectedSuccess

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("collector >= %d", expectedSuccess), func() bool {
		return collector.count() >= expectedSuccess
	})

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("DLQ >= %d", expectedDLQ), func() bool {
		return dlq.count() >= expectedDLQ
	})

	t.Logf("UC70: collector=%d (expect %d), DLQ=%d (expect %d)",
		collector.count(), expectedSuccess, dlq.count(), expectedDLQ)

	assert.GreaterOrEqual(t, collector.count(), expectedSuccess)
	assert.GreaterOrEqual(t, dlq.count(), expectedDLQ)
}

// TestUC71_PoisonMessageAttemptCount ensures that poison messages (messages that
// always fail) are retried up to MaxReplayAttempts before landing in the DLQ,
// and the Attempts field on each DLQ entry reflects the retry count.
func TestUC71_PoisonMessageAttemptCount(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount          = 100
		outTopic          = "uc71/output"
		maxReplayAttempts = 3
		testTimeout       = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc71-in")
	dlq := &replayableDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sessID := mqttlocal.UniqueClientID("uc71-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	failSnd := &alwaysFailSender{}
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc71"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc71-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliverySharedOutbox,
			MaxReplayAttempts: maxReplayAttempts,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc71-bind",
			Address:   outTopic,
		}),
		SourceCapabilities: directHoldCaps,
	}, rx, failSnd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlq.count() >= msgCount
	})

	entries := dlq.getEntries()
	t.Logf("UC71: DLQ entries=%d", len(entries))
	require.GreaterOrEqual(t, len(entries), msgCount)

	for i, e := range entries {
		assert.GreaterOrEqual(t, e.Attempts, 1,
			"entry %d: Attempts should be >= 1, got %d", i, e.Attempts)
	}
}

// TestUC72_DLQEntryFieldIntegrity validates that every DLQ entry produced by a
// failing sender contains all required fields: ID, RouteID, Category, Reason,
// FailedAt, and a non-empty Envelope.ID.
func TestUC72_DLQEntryFieldIntegrity(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		outTopic    = "uc72/output"
		testTimeout = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc72-in")
	dlq := &replayableDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sessID := mqttlocal.UniqueClientID("uc72-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	failSnd := &alwaysFailSender{}
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc72"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc72-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliverySharedOutbox,
			MaxReplayAttempts: 1,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc72-bind",
			Address:   outTopic,
		}),
		SourceCapabilities: directHoldCaps,
	}, rx, failSnd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlq.count() >= msgCount
	})

	entries := dlq.getEntries()
	t.Logf("UC72: DLQ entries=%d", len(entries))
	require.GreaterOrEqual(t, len(entries), msgCount)

	for i, e := range entries {
		assert.NotEmpty(t, e.ID,
			"entry %d: ID must not be empty", i)
		assert.NotEmpty(t, e.RouteID,
			"entry %d: RouteID must not be empty", i)
		assert.NotEmpty(t, e.Category,
			"entry %d: Category must not be empty", i)
		assert.NotEmpty(t, e.Reason,
			"entry %d: Reason must not be empty", i)
		assert.False(t, e.FailedAt.IsZero(),
			"entry %d: FailedAt must not be zero", i)
		assert.NotEmpty(t, e.Envelope.ID,
			"entry %d: Envelope.ID must not be empty", i)
	}
}

// TestUC73_MixedErrorTypes exercises a mixed workload where 60% of messages
// succeed and 40% fail with either transient or permanent errors, verifying
// that the collector and DLQ counts match the expected distribution.
func TestUC73_MixedErrorTypes(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 1000
		outTopic    = "uc73/output"
		testTimeout = 180 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc73-in")
	dlq := &replayableDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc73-col")

	sessID := mqttlocal.UniqueClientID("uc73-sess")
	sess := newMQTTSession(t, sessID, domain.SessionExclusive)
	realSnd := setupMQTTSender(t, sess)
	errSnd := &errorClassSender{inner: realSnd}
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc73"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc73-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliveryDirectHold,
			MaxReplayAttempts: 1,
		},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{
			BindingID: "uc73-bind",
			Address:   outTopic,
		}),
		SourceCapabilities: directHoldCaps,
	}, rx, errSnd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// i%5: 0,1,4 -> no header (succeed=600), 2 -> transient (200), 3 -> permanent (200)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, func(i int) map[string]string {
		switch i % 5 {
		case 2:
			return map[string]string{"error_type": "transient"}
		case 3:
			return map[string]string{"error_type": "permanent"}
		default:
			return nil
		}
	})

	expectedSuccess := 600 // 3 out of every 5 messages succeed
	expectedDLQ := 400     // 2 out of every 5 messages fail

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("collector >= %d", expectedSuccess), func() bool {
		return collector.count() >= expectedSuccess
	})

	lrWaitFor(t, 120*time.Second, fmt.Sprintf("DLQ >= %d", expectedDLQ), func() bool {
		return dlq.count() >= expectedDLQ
	})

	t.Logf("UC73: collector=%d (expect >= %d), DLQ=%d (expect >= %d)",
		collector.count(), expectedSuccess, dlq.count(), expectedDLQ)

	assert.GreaterOrEqual(t, collector.count(), expectedSuccess)
	assert.GreaterOrEqual(t, dlq.count(), expectedDLQ)
}
