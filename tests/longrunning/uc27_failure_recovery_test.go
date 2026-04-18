//go:build longrunning

package longrunning_test

import (
	"context"
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
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// =========================================================================
// UC27: Intermittent Send Failures
//
// 3,000 messages sent through SQS -> bridge -> MQTT with a faulty sender
// that fails 20% of calls. At-least-once: all delivered eventually.
// DLQ must be empty because the failures are transient.
// =========================================================================

func TestUC27_Intermittent_SendFailures(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 3000
		pollTimeout = 600 * time.Second // 10 min: 20% failure rate needs longer for all retries
	)

	inQueueURL, inClient := setupSQSQueue(t, "uc27-in")
	collector := newMQTTCollector(t, "uc27/output/data", "uc27-col")

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc27-bridge"), domain.SessionEphemeral)
	realSender := setupMQTTSender(t, sess)
	faulty := newFaultySender(realSender, 20)
	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          inQueueURL,
		Client:            sqslocal.Client(t),
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30, // 30s: prevents premature SQS redelivery during retries
	}, testLogger(t))
	require.NoError(t, err)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc27-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc27-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        100,
			MaxReplayAttempts:  50,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc27/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, faulty, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, inClient, inQueueURL, msgCount, nil)

	start := time.Now()
	lastLog := time.Now()
	// Wait for unique payloads (not total count, which includes SQS
	// redelivery duplicates caused by visibility timeout expiry).
	lrWaitFor(t, pollTimeout, fmt.Sprintf("unique payloads >= %d", msgCount), func() bool {
		msgs := collector.getMessages()
		seen := make(map[string]struct{}, len(msgs))
		for _, m := range msgs {
			seen[string(m.Payload)] = struct{}{}
		}
		if time.Since(lastLog) > 10*time.Second {
			t.Logf("UC27: progress unique=%d/%d total=%d, elapsed=%v, faulty_calls=%d",
				len(seen), msgCount, len(msgs), time.Since(start).Truncate(time.Second), faulty.calls.Load())
			lastLog = time.Now()
		}
		return len(seen) >= msgCount
	})

	time.Sleep(2 * time.Second) // SYNC: let in-flight retries settle

	got := collector.count()
	msgs := collector.getMessages()
	unique := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		unique[string(m.Payload)] = true
	}

	// Log diagnostics BEFORE assertions so we always get visibility.
	t.Logf("UC27: delivered %d messages (%d unique), DLQ=%d, sender calls=%d",
		got, len(unique), dlqStore.count(), faulty.calls.Load())
	assert.Equal(t, 0, dlqStore.count(), "DLQ should be empty — all errors are transient")

	require.GreaterOrEqual(t, got, msgCount,
		"collector should have at least %d messages, got %d", msgCount, got)
	require.GreaterOrEqual(t, len(unique), msgCount,
		"should have at least %d unique payloads", msgCount)
}

// =========================================================================
// UC28: Visibility Timeout Race
//
// 500 messages with SQS visibility=5s and a variable-delay processor
// (0-8s based on seq%8). AutoExtend=true. All 500 unique payloads arrive.
// =========================================================================

// variableDelayProcessor adds a delay based on sequence number modulo.
type variableDelayProcessor struct {
	modulo int
}

func (p *variableDelayProcessor) Name() string { return "variable-delay" }

func (p *variableDelayProcessor) Process(
	ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc,
) error {
	seq := extractSeq(env)
	delay := time.Duration(seq%p.modulo) * time.Second
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return next(ctx, env)
}

// extractSeq parses a seq number from the payload JSON like {"seq":42}.
func extractSeq(env *domain.Envelope) int {
	body := string(env.Payload)
	var seq int
	_, _ = fmt.Sscanf(body, `{"seq":%d}`, &seq)
	return seq
}

func TestUC28_VisibilityTimeout_Race(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		pollTimeout = 180 * time.Second
	)

	// Create SQS queue with short visibility timeout (5s).
	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("uc28-in")
	inQueueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "5",
	})

	collector := newMQTTCollector(t, "uc28/output/data", "uc28-col")

	dlqStore := &lrDLQStore{}

	// Receiver with visibility=5s and AutoExtend=true.
	ep := sqslocal.Endpoint(t)
	autoExtend := true
	sqsRx, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          inQueueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	require.NoError(t, err, "newSQSReceiver for UC28")

	sess := setupMQTTSession(t, uniqueID("uc28-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc28-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc28-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        50,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{&variableDelayProcessor{modulo: 8}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc28/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	sendBulkToSQS(t, client, inQueueURL, msgCount, nil)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d unique", msgCount), func() bool {
		msgs := collector.getMessages()
		unique := make(map[string]bool, len(msgs))
		for _, m := range msgs {
			unique[string(m.Payload)] = true
		}
		return len(unique) >= msgCount
	})

	time.Sleep(2 * time.Second) // SYNC: let in-flight deliveries settle

	msgs := collector.getMessages()
	unique := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		unique[string(m.Payload)] = true
	}

	require.GreaterOrEqual(t, len(unique), msgCount,
		"should have at least %d unique payloads, got %d", msgCount, len(unique))

	t.Logf("UC28: %d total messages, %d unique payloads, DLQ=%d",
		len(msgs), len(unique), dlqStore.count())
}

// =========================================================================
// UC29: Message TTL Expiry
//
// 500 messages injected with ExpiresAt already in the past.
// Policy: OnExpired=ExpiredDLQ. All 500 go to DLQ with expired category.
// 0 reach MQTT.
// =========================================================================

func TestUC29_MessageTTL_Expiry(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		pollTimeout = 120 * time.Second
	)

	collector := newMQTTCollector(t, "uc29/output/data", "uc29-col")

	dlqStore := &lrDLQStore{}

	sess := setupMQTTSession(t, uniqueID("uc29-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc29-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc29-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        50,
			OnExpired:          domain.ExpiredDLQ,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc29/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	// Use noopReceiver since we inject messages directly.
	require.NoError(t, rt.AddRoute(routeCfg, &noopReceiver{}, mqttSender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// Inject messages with ExpiresAt in the past (already expired).
	// The route runner checks env.IsExpired() before processing.
	for i := 0; i < msgCount; i++ {
		env := &domain.Envelope{
			ID:        fmt.Sprintf("uc29-msg-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			ExpiresAt: time.Now().Add(-1 * time.Second),
		}
		err := rt.Inject(ctx, "uc29-route", env)
		require.NoError(t, err, "inject message %d", i)
	}

	lrWaitFor(t, pollTimeout, fmt.Sprintf("DLQ >= %d", msgCount), func() bool {
		return dlqStore.count() >= msgCount
	})

	time.Sleep(2 * time.Second) // NEGATIVE: verify expired messages did not reach MQTT

	gotDLQ := dlqStore.count()
	gotMQTT := collector.count()

	require.Equal(t, msgCount, gotDLQ,
		"DLQ should have exactly %d entries, got %d", msgCount, gotDLQ)
	require.Equal(t, 0, gotMQTT,
		"MQTT collector should have 0 messages, got %d", gotMQTT)

	// Verify DLQ entries have expired category.
	entries := dlqStore.getEntries()
	for i, entry := range entries {
		assert.Equal(t, string(domain.ErrorExpired), entry.Category,
			"DLQ entry %d should have 'expired' category, got %q", i, entry.Category)
	}

	t.Logf("UC29: DLQ=%d (all expired), MQTT=%d", gotDLQ, gotMQTT)
}
