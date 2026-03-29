//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// poisonProcessor rejects messages whose "poison" header is set to "true"
// with a permanent error. Normal messages pass through unchanged.
type poisonProcessor struct{}

func (p *poisonProcessor) Name() string { return "poison-filter" }

func (p *poisonProcessor) Process(
	ctx context.Context,
	env *domain.Envelope,
	next ports.ProcessorFunc,
) error {
	v, ok := domain.GetHeaderString(env.Headers, "poison")
	if ok && v == "true" {
		return domain.ErrInvalidPayload.WithMessage("poison message rejected")
	}
	return next(ctx, env)
}

// TestUC6_BurstBackpressure validates that the bridge correctly handles
// a burst of mixed normal and poison messages under a tight MaxInFlight
// constraint, routing failures to the DLQ while delivering valid messages
// to the MQTT output topic.
//
// Topology:
//
//	SQS-IN -> [Bridge] (MaxInFlight=50, poison processor) -> MQTT "uc6/output/data"
//	                                                       -> DLQ (poison messages)
//
// Volume: 3,000 messages total (2,500 normal + 500 poison interleaved).
// Verification: MQTT collector has 2,500, DLQ has 500, total = 3,000.
func TestUC6_BurstBackpressure(t *testing.T) {
	const (
		totalCount  = 3000
		normalCount = 2500
		poisonCount = 500
		pollTimeout     = 120 * time.Second
	)

	// -- Infrastructure ---------------------------------------------------
	inQueueURL, inClient := setupSQSQueue(t, "uc6-in")
	collector := newMQTTCollector(t, "uc6/output/data", "uc6-col")
	time.Sleep(300 * time.Millisecond)

	// -- DLQ store --------------------------------------------------------
	dlqStore := &lrDLQStore{}

	// -- Bridge: SQS-IN -> MQTT uc6/output/data ---------------------------
	sess := setupMQTTSession(t, uniqueID("uc6-bridge"), domain.SessionEphemeral)
	mqttSender := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc6-bridge"),
		goruntime.WithDLQStore(dlqStore),
	)

	routeCfg := goruntime.RouteConfig{
		ID: "uc6-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			MaxInFlight:        50,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{&poisonProcessor{}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc6/output/data"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSender, nil, nil))

	// -- Start bridge ------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// -- Send mixed messages to SQS-IN ------------------------------------
	// Every 6th message (i%6==5) is poison (gives ~500 out of 3000).
	// That yields exactly 500 poison messages (indices 5,11,17,...,2999).
	sendBulkToSQS(t, inClient, inQueueURL, totalCount,
		func(i int) map[string]string {
			if i%6 == 5 {
				return map[string]string{"poison": "true"}
			}
			return nil
		},
	)

	// -- Wait for total accounting: MQTT + DLQ == 3,000 -------------------
	lrWaitFor(t, pollTimeout,
		fmt.Sprintf("MQTT(%d) + DLQ(%d) = %d", normalCount, poisonCount, totalCount),
		func() bool {
			return collector.count()+dlqStore.count() >= totalCount
		},
	)

	// Allow a brief settling period for any in-flight deliveries.
	time.Sleep(2 * time.Second)

	// -- Verification: exact counts ----------------------------------------
	gotNormal := collector.count()
	gotPoison := dlqStore.count()
	gotTotal := gotNormal + gotPoison

	require.Equal(t, totalCount, gotTotal,
		"total accounting: MQTT(%d) + DLQ(%d) = %d, want %d",
		gotNormal, gotPoison, gotTotal, totalCount)

	require.Equal(t, normalCount, gotNormal,
		"MQTT collector should have exactly %d normal messages, got %d",
		normalCount, gotNormal)

	require.Equal(t, poisonCount, gotPoison,
		"DLQ should have exactly %d poison entries, got %d",
		poisonCount, gotPoison)

	// -- Verification: MQTT collector messages are non-poison ----------------
	msgs := collector.getMessages()
	for idx, msg := range msgs {
		require.True(t, len(msg.Payload) > 0,
			"MQTT message %d has empty payload", idx)
		// Normal messages must NOT have the poison header set to "true".
		v, _ := domain.GetHeaderString(msg.Headers, "poison")
		require.NotEqual(t, "true", v,
			"MQTT message %d has poison=true header but should be normal", idx)
	}

	// -- Verification: DLQ entries are poison messages ----------------------
	dlqEntries := dlqStore.getEntries()
	for idx, entry := range dlqEntries {
		require.True(t, len(entry.Envelope.Payload) > 0,
			"DLQ entry %d has empty payload", idx)
		// Verify the error classification is not empty.
		require.NotEmpty(t, entry.Category,
			"DLQ entry %d should have an error category", idx)
	}

	t.Logf("UC6: Burst backpressure verified -- %d normal to MQTT, %d poison to DLQ, %d total",
		gotNormal, gotPoison, gotTotal)
}
