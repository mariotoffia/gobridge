package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/outbox"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Outbox Drainer → Real SQS Sender Integration Tests
//
// Validates the full Outbox Drainer pipeline with a real SQS sender
// backed by the local AWS emulator, exercising the Persist → Claim → Send → SQS
// lifecycle end-to-end.
//
// Summary:
// ┌──────┬───────────────────────────────────────────────────────────────┐
// │ Test │ Description                                                   │
// ├──────┼───────────────────────────────────────────────────────────────┤
// │ SQ1  │ Full cycle: persist 5 records → drain → verify in SQS        │
// │ SQ2  │ Expired record skips SQS, lands in DLQ store                 │
// │ SQ3  │ DispatchHeaders survive outbox → SQS message attributes      │
// └──────┴───────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_OutboxDrainer_RealSQSSender_FullCycle validates the
// complete pipeline: persist records into DynamoDB outbox, drain them
// through a real SQS sender, and verify all messages arrive in the queue.
//
// Scenario:
//
//	┌──────────┐     ┌──────────────┐     ┌──────────┐     ┌───────────┐
//	│ Persist  │────▶│ DynamoDB     │────▶│ Drainer  │────▶│ SQS Queue │
//	│ 5 recs   │     │ OutboxStore  │     │ Claim+   │     │ (emulated │
//	└──────────┘     └──────────────┘     │ Send     │     │  verify)  │
//	                                      │ Complete │     └───────────┘
//	                                      └──────────┘
func TestIntegration_OutboxDrainer_RealSQSSender_FullCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	store := newDDBOutboxStore(t, "sq1")
	queueURL, sqsClient := setupSQSQueue(t, "sq1")

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Endpoint: flocilocal.Endpoint(t),
		Region:   "us-west-1",
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("create SQS sender: %v", err)
	}

	ctx := context.Background()
	tok := persistence.LeaseToken{Version: 1, Owner: "drainer-sq1"}
	pk := persistence.OutboxPartitionKey("sess-sq1", "")

	const recordCount = 5
	for i := 0; i < recordCount; i++ {
		rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         uniqueID("sq1-rec"),
			EnvelopeID: fmt.Sprintf("env-sq1-%d", i),
			BindingID:  "bind-sq1",
			SessionID:  "sess-sq1",
			RouteID:    "route-sq1",
			Address:    queueURL,
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      fmt.Sprintf("env-sq1-%d", i),
				Subject: "test/sqs/full-cycle",
				Payload: []byte(fmt.Sprintf(`{"index":%d}`, i)),
			}),
		})
		if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist record %d: %v", i, err)
		}
	}

	drainer := outbox.New(outbox.Config{
		OutboxStore:    store,
		Sender:         sender,
		RouteID:        "route-sq1",
		PartitionKey:   pk,
		Policy:         routing.RoutePolicy{SendTimeout: 10 * time.Second, MaxReplayAttempts: 3},
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (persistence.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 10*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	bodies := pollSQS(t, sqsClient, queueURL, recordCount, 15*time.Second)
	if len(bodies) != recordCount {
		t.Fatalf("expected %d messages in SQS, got %d", recordCount, len(bodies))
	}

	rxBodies := make(map[string]bool, len(bodies))
	for _, b := range bodies {
		rxBodies[b] = true
	}
	for i := 0; i < recordCount; i++ {
		want := fmt.Sprintf(`{"index":%d}`, i)
		if !rxBodies[want] {
			t.Errorf("missing body %s in SQS messages", want)
		}
	}

	pending, err := store.QueryPending(ctx, pk, 100)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(pending))
	}
}

// TestIntegration_OutboxDrainer_RealSQSSender_ExpiredToDLQ validates that
// an expired record is NOT sent to SQS but IS routed to the DLQ store.
//
// Scenario:
//
//	┌──────────┐     ┌──────────────┐     ┌──────────┐
//	│ Persist  │────▶│ DynamoDB     │────▶│ Drainer  │
//	│ expired  │     │ OutboxStore  │     │ detects  │
//	│ record   │     └──────────────┘     │ expired  │
//	└──────────┘                          └────┬─────┘
//	                                           │
//	                         ┌─────────────────┼─────────────────┐
//	                         ▼                                   ▼
//	                  ┌──────────┐                        ┌──────────┐
//	                  │ SQS      │ ← 0 messages           │ DLQ      │ ← 1 entry
//	                  │ Queue    │                        │ Store    │
//	                  └──────────┘                        └──────────┘
func TestIntegration_OutboxDrainer_RealSQSSender_ExpiredToDLQ(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	store := newDDBOutboxStore(t, "sq2")
	queueURL, sqsClient := setupSQSQueue(t, "sq2")

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Endpoint: flocilocal.Endpoint(t),
		Region:   "us-west-1",
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("create SQS sender: %v", err)
	}

	dlqStore := &e2eDLQStore{}
	tok := persistence.LeaseToken{Version: 1, Owner: "drainer-sq2"}
	pk := persistence.OutboxPartitionKey("sess-sq2", "")
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour)
	rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         uniqueID("sq2-rec"),
		EnvelopeID: "env-sq2",
		BindingID:  "bind-sq2",
		SessionID:  "sess-sq2",
		RouteID:    "route-sq2",
		Address:    "test/sqs/expired",
		ExpiresAt:  past,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:        "env-sq2",
			Subject:   "test/sqs/expired",
			Payload:   []byte(`{"expired":"should-not-reach-sqs"}`),
			ExpiresAt: past,
		}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	dlqRouter := dlq.NewFromConfig(dlq.Config{
		Store: dlqStore,
	})

	drainer := outbox.New(outbox.Config{
		OutboxStore:    store,
		Sender:         sender,
		DLQ:            dlqRouter,
		RouteID:        "route-sq2",
		PartitionKey:   pk,
		Policy:         routing.RoutePolicy{SendTimeout: 10 * time.Second, MaxReplayAttempts: 3, OnExpired: routing.ExpiredDLQ},
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (persistence.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	// Verify SQS received nothing.
	bodies := pollSQS(t, sqsClient, queueURL, 1, 3*time.Second)
	if len(bodies) != 0 {
		t.Fatalf("expected 0 messages in SQS for expired record, got %d", len(bodies))
	}

	// Verify DLQ received the expired record.
	if dlqStore.count() != 1 {
		t.Fatalf("expected 1 DLQ entry for expired record, got %d", dlqStore.count())
	}

	pending, _ := store.QueryPending(ctx, pk, 10)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after expired drain, got %d", len(pending))
	}
}

// TestIntegration_OutboxDrainer_RealSQSSender_HeaderPreservation validates
// that DispatchHeaders set on an OutboxRecord survive the full
// outbox → drainer → SQS sender pipeline and appear as SQS message
// attributes on the received message.
//
// Scenario:
//
//	┌──────────────────┐     ┌──────────────┐     ┌──────────┐     ┌───────────┐
//	│ Persist record   │────▶│ DynamoDB     │────▶│ Drainer  │────▶│ SQS Queue │
//	│ DispatchHeaders: │     │ OutboxStore  │     │ merges   │     │           │
//	│  X-Trace: abc    │     └──────────────┘     │ headers  │     │  verify   │
//	│  X-Tenant: t1    │                          │ into env │     │  attrs    │
//	└──────────────────┘                          └──────────┘     └───────────┘
func TestIntegration_OutboxDrainer_RealSQSSender_HeaderPreservation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	store := newDDBOutboxStore(t, "sq3")
	queueURL, sqsClient := setupSQSQueue(t, "sq3")

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Endpoint: flocilocal.Endpoint(t),
		Region:   "us-west-1",
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("create SQS sender: %v", err)
	}

	tok := persistence.LeaseToken{Version: 1, Owner: "drainer-sq3"}
	pk := persistence.OutboxPartitionKey("sess-sq3", "")
	ctx := context.Background()

	customHeaders := map[string]any{
		"X-Trace-ID": "trace-abc-123",
		"X-Tenant":   "tenant-one",
		"X-Priority": "high",
	}

	rec := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:              uniqueID("sq3-rec"),
		EnvelopeID:      "env-sq3",
		BindingID:       "bind-sq3",
		SessionID:       "sess-sq3",
		RouteID:         "route-sq3",
		Address:         queueURL,
		DispatchHeaders: customHeaders,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-sq3",
			Subject: "test/sqs/headers",
			Payload: []byte(`{"headers":"preservation-test"}`),
		}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	drainer := outbox.New(outbox.Config{
		OutboxStore:    store,
		Sender:         sender,
		RouteID:        "route-sq3",
		PartitionKey:   pk,
		Policy:         routing.RoutePolicy{SendTimeout: 10 * time.Second, MaxReplayAttempts: 3},
		Strategy:       persistence.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (persistence.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 10*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	// Poll SQS with MessageAttributeNames: ["All"] to retrieve all attributes.
	var received []sqstypes.Message
	e2eWaitFor(t, 15*time.Second, "SQS message with attributes", func() bool {
		out, pollErr := sqsClient.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:              &queueURL,
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		if pollErr != nil {
			t.Logf("poll error: %v", pollErr)
			return false
		}
		received = append(received, out.Messages...)
		return len(received) >= 1
	})

	if len(received) == 0 {
		t.Fatal("expected at least 1 message in SQS, got 0")
	}

	msg := received[0]
	if msg.Body == nil || *msg.Body != `{"headers":"preservation-test"}` {
		t.Fatalf("body mismatch: got %q", derefMsgBody(msg.Body))
	}

	// Verify each custom header survived as an SQS message attribute.
	for key, expectedVal := range customHeaders {
		attr, ok := msg.MessageAttributes[key]
		if !ok {
			t.Errorf("missing SQS message attribute %q", key)
			continue
		}
		if attr.StringValue == nil {
			t.Errorf("attribute %q has nil StringValue", key)
			continue
		}
		if *attr.StringValue != expectedVal {
			t.Errorf("attribute %q: got %q, want %q", key, *attr.StringValue, expectedVal)
		}
	}

	// The Subject header should also be present (set from Address).
	if subj, ok := msg.MessageAttributes["Subject"]; ok {
		if subj.StringValue == nil || *subj.StringValue != "test/sqs/headers" {
			t.Errorf("Subject attribute: got %q, want %q", derefMsgBody(subj.StringValue), "test/sqs/headers")
		}
	} else {
		t.Error("missing Subject message attribute")
	}

	pending, _ := store.QueryPending(ctx, pk, 10)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(pending))
	}
}

func derefMsgBody(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
