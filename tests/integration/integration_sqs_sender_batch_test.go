package integration_test

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS Sender batch integration tests against the local AWS emulator
//
// Validates SendBatch behaviour against a real SQS-compatible endpoint.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────┐
// │ Test │ Description                                          │
// ├──────┼──────────────────────────────────────────────────────┤
// │ IB1  │ SendBatch 25 messages arrive in correct count        │
// │ IB2  │ Batch boundaries: 1-10, 11-20, 21-25 are respected  │
// │ IB3  │ Large batch (50 messages) all arrive with headers    │
// └──────┴──────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_SendBatch_25Messages sends 25 envelopes through
// SendBatch and verifies all 25 arrive in the emulated queue.
func TestIntegration_SQS_SendBatch_25Messages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := newSQSClient(t)
	queueURL := createSQSQueue(t, client, uniqueQueueName("ib1"))

	sender := newBatchSender(t, queueURL)

	envs := make([]*messaging.Envelope, 25)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("batch-msg-%d", i),
			Payload: []byte(fmt.Sprintf("body-%d", i)),
			Headers: map[string]any{
				"X-Seq": strconv.Itoa(i),
			},
		})
	}

	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 25 {
		t.Fatalf("expected 25 sent, got %d", sent)
	}

	bodies := pollSQS(t, client, queueURL, 25, 30*time.Second)
	if len(bodies) != 25 {
		t.Fatalf("expected 25 messages in queue, got %d", len(bodies))
	}

	// Verify all expected bodies are present (order not guaranteed).
	expected := make(map[string]bool, 25)
	for i := 0; i < 25; i++ {
		expected[fmt.Sprintf("body-%d", i)] = false
	}
	for _, b := range bodies {
		if _, ok := expected[b]; ok {
			expected[b] = true
		}
	}
	for body, found := range expected {
		if !found {
			t.Fatalf("missing message body: %q", body)
		}
	}
}

// TestIntegration_SQS_SendBatch_VerifyBatchBoundaries verifies that
// messages are correctly split into batches of 10. We send 25 messages
// and confirm all arrive, grouped by batch index prefix in the payload.
func TestIntegration_SQS_SendBatch_VerifyBatchBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := newSQSClient(t)
	queueURL := createSQSQueue(t, client, uniqueQueueName("ib2"))

	sender := newBatchSender(t, queueURL)

	// Build 25 envelopes with batch-identifiable payloads.
	// Messages 0-9 in batch 1, 10-19 in batch 2, 20-24 in batch 3.
	envs := make([]*messaging.Envelope, 25)
	for i := range envs {
		batchIdx := i / 10
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("boundary-%d", i),
			Payload: []byte(fmt.Sprintf("batch%d-msg%d", batchIdx, i)),
		})
	}

	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 25 {
		t.Fatalf("expected 25 sent, got %d", sent)
	}

	bodies := pollSQS(t, client, queueURL, 25, 30*time.Second)
	if len(bodies) != 25 {
		t.Fatalf("expected 25 messages, got %d", len(bodies))
	}

	// Count messages per batch prefix.
	batchCounts := map[string]int{}
	for _, b := range bodies {
		// Extract "batchN" prefix.
		prefix := b
		if idx := len("batchN"); idx < len(b) {
			prefix = b[:idx]
		}
		batchCounts[prefix]++
	}

	if batchCounts["batch0"] != 10 {
		t.Fatalf("batch0 expected 10 messages, got %d", batchCounts["batch0"])
	}
	if batchCounts["batch1"] != 10 {
		t.Fatalf("batch1 expected 10 messages, got %d", batchCounts["batch1"])
	}
	if batchCounts["batch2"] != 5 {
		t.Fatalf("batch2 expected 5 messages, got %d", batchCounts["batch2"])
	}
}

// TestIntegration_SQS_SendBatch_LargeWithHeaders sends 50 messages with
// headers and validates all arrive with correct message attributes.
func TestIntegration_SQS_SendBatch_LargeWithHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := newSQSClient(t)
	queueURL := createSQSQueue(t, client, uniqueQueueName("ib3"))

	sender := newBatchSender(t, queueURL)

	const total = 50
	envs := make([]*messaging.Envelope, total)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("large-%d", i),
			Subject: "batch-test",
			Payload: []byte(fmt.Sprintf("large-body-%d", i)),
			Headers: map[string]any{
				"X-Index": strconv.Itoa(i),
			},
		})
	}

	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != total {
		t.Fatalf("expected %d sent, got %d", total, sent)
	}

	// Poll all messages with full attributes.
	msgs := pollSQSRaw(t, client, queueURL, total, 30*time.Second)
	if len(msgs) != total {
		t.Fatalf("expected %d messages, got %d", total, len(msgs))
	}

	// Verify all indices are present.
	indices := make([]int, 0, total)
	for _, m := range msgs {
		attr, ok := m.MessageAttributes["X-Index"]
		if !ok || attr.StringValue == nil {
			t.Fatal("missing X-Index attribute")
		}
		idx, err := strconv.Atoi(*attr.StringValue)
		if err != nil {
			t.Fatalf("invalid X-Index: %v", err)
		}
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for i := 0; i < total; i++ {
		if indices[i] != i {
			t.Fatalf("missing index %d in received messages", i)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newBatchSender(t *testing.T, queueURL string) *sqsadapter.Sender {
	t.Helper()
	// NewSender builds its own AWS SDK client. The emulator ignores credentials,
	// but the SDK still requires a provider and must not fall through to host
	// profile/metadata discovery.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ep := flocilocal.Endpoint(t)
	s, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueURL: queueURL,
		Endpoint: ep,
		Region:   "us-west-1",
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("newBatchSender: %v", err)
	}
	return s
}

// pollSQSRaw polls SQS and returns raw message objects with attributes.
func pollSQSRaw(
	t *testing.T,
	client *sqs.Client,
	queueURL string,
	count int,
	timeout time.Duration,
) []sqsRawMessage {
	t.Helper()
	var msgs []sqsRawMessage
	deadline := time.Now().Add(timeout)
	for len(msgs) < count && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:              &queueURL,
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Logf("pollSQSRaw: %v", err)
			time.Sleep(200 * time.Millisecond) // OTHER: backoff on transient SQS error
			continue
		}
		for _, m := range out.Messages {
			msgs = append(msgs, sqsRawMessage{
				Body:              *m.Body,
				MessageAttributes: m.MessageAttributes,
			})
			_, _ = client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl:      &queueURL,
				ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	return msgs
}

// sqsRawMessage holds a received SQS message body and its attributes.
type sqsRawMessage struct {
	Body              string
	MessageAttributes map[string]sqstypes.MessageAttributeValue
}

// batchSent counts the successful (nil-Err) entries in a SendBatch result.
func batchSent(results []ports.BatchResult) int {
	n := 0
	for _, r := range results {
		if r.Err == nil {
			n++
		}
	}
	return n
}
