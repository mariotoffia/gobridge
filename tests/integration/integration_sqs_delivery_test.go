package integration_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS Delivery Lifecycle Integration Tests
//
// Validates the full delivery lifecycle (Ack, Retry, Extend) and header
// round-trip against a real SQS-compatible endpoint (ElasticMQ in Docker).
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────────┐
// │ Test │ Description                                              │
// ├──────┼──────────────────────────────────────────────────────────┤
// │ ID1  │ Ack removes message from queue                           │
// │ ID2  │ Retry with delay makes message reappear                  │
// │ ID3  │ Extend prevents premature redelivery                     │
// │ ID4  │ Custom headers survive send→receive round-trip           │
// │ ID5  │ Auto-extend keeps message invisible during processing    │
// └──────┴──────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_Delivery_AckRemovesMessage validates that calling Ack
// on a delivery deletes the message from the queue so it cannot be re-received.
//
// Scenario:
//
//	Client ──send──▶ [Queue vt=30s] ──recv──▶ Receiver ──Ack──▶ [Queue empty]
//	                                          Client ──poll──▶ [no messages]
func TestIntegration_SQS_Delivery_AckRemovesMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("id1")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "30",
	})

	sendToSQS(t, client, queueURL, `{"ack":"test"}`, nil)

	receiver := newSQSReceiverWithVisibility(t, queueURL, 30)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		if err := del.Ack(ctx); err != nil {
			t.Fatalf("Ack failed: %v", err)
		}
		cancel()
		return nil
	})

	remaining := pollSQSNoDelete(t, client, queueURL, 3*time.Second)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 messages after Ack, got %d", len(remaining))
	}
}

// TestIntegration_SQS_Delivery_RetryMakesMessageReappear validates that calling
// Retry with a zero delay makes the message immediately available for redelivery.
//
// Scenario:
//
//	Client ──send──▶ [Queue vt=30s] ──recv──▶ Receiver ──Retry(0)──▶ [Queue]
//	                                          Receiver ──recv(2nd)──▶ callback
func TestIntegration_SQS_Delivery_RetryMakesMessageReappear(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("id2")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "30",
	})

	sendToSQS(t, client, queueURL, `{"retry":"test"}`, nil)

	autoExtend := false
	ep := sqslocal.Endpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       1,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var deliveryCount int
	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		deliveryCount++
		if deliveryCount == 1 {
			if err := del.Retry(ctx, 0, nil); err != nil {
				t.Fatalf("Retry failed: %v", err)
			}
			return nil
		}
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if deliveryCount < 2 {
		t.Fatalf("expected at least 2 deliveries after Retry, got %d", deliveryCount)
	}
}

// TestIntegration_SQS_Delivery_ExtendPreventsRedelivery validates that calling
// Extend pushes the visibility timeout forward, preventing premature redelivery.
//
// Scenario:
//
//	Client ──send──▶ [Queue vt=3s] ──recv──▶ Receiver ──Extend(+30s)──▶
//	... 4s (past original vt) ...
//	Receiver ──Ack──▶ [Queue empty]
//	Client ──poll──▶ [no redelivery occurred]
func TestIntegration_SQS_Delivery_ExtendPreventsRedelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("id3")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "3",
	})

	sendToSQS(t, client, queueURL, `{"extend":"test"}`, nil)

	autoExtend := false
	ep := sqslocal.Endpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       1,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 3,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var deliveryCount int
	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		deliveryCount++
		if deliveryCount == 1 {
			if err := del.Extend(ctx, time.Now().Add(30*time.Second)); err != nil {
				t.Fatalf("Extend failed: %v", err)
			}
			time.Sleep(4 * time.Second) // ESSENTIAL: exceed original 3s visibility timeout to prove Extend worked
			if err := del.Ack(ctx); err != nil {
				t.Fatalf("Ack after Extend failed: %v", err)
			}
			cancel()
			return nil
		}
		t.Fatal("unexpected redelivery after Extend")
		return nil
	})

	if deliveryCount != 1 {
		t.Fatalf("expected exactly 1 delivery (Extend prevented redelivery), got %d", deliveryCount)
	}
}

// TestIntegration_SQS_HeaderRoundTrip validates that custom headers set on an
// envelope survive the Sender → SQS → Receiver round-trip as message attributes.
//
// Scenario:
//
//	Sender ──send(headers)──▶ [Queue] ──recv──▶ Receiver
//	                                              ↓
//	                                    Verify headers match
func TestIntegration_SQS_HeaderRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	queueURL, _ := setupSQSQueue(t, "id4")
	sender := newSQSSender(t, queueURL)

	env := &messaging.Envelope{
		ID:      "hdr-roundtrip-1",
		Subject: "test-subject-hdr",
		Payload: []byte(`{"header":"roundtrip"}`),
		Headers: map[string]any{
			"X-Custom-String": "hello",
			"X-Trace-ID":      "trace-12345",
			"X-Numeric":       "42",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	receiver := newSQSReceiver(t, queueURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var received *messaging.Envelope
	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		received = del.Envelope()
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if received == nil {
		t.Fatal("no envelope received")
	}
	if string(received.Payload) != `{"header":"roundtrip"}` {
		t.Fatalf("payload mismatch: got %q", string(received.Payload))
	}

	checkHeader := func(key, want string) {
		t.Helper()
		got, ok := received.Headers[key].(string)
		if !ok {
			t.Fatalf("header %q not found or not a string; headers=%v", key, received.Headers)
		}
		if got != want {
			t.Fatalf("header %q: got %q, want %q", key, got, want)
		}
	}
	checkHeader("X-Custom-String", "hello")
	checkHeader("X-Trace-ID", "trace-12345")
	checkHeader("X-Numeric", "42")

	if received.Subject != "test-subject-hdr" {
		t.Fatalf("subject mismatch: got %q, want %q", received.Subject, "test-subject-hdr")
	}
}

// TestIntegration_SQS_AutoExtendKeepsMessageInvisible validates that auto-extend
// prevents a message from becoming visible while processing takes longer than the
// visibility timeout.
//
// Scenario:
//
//	Client ──send──▶ [Queue vt=4s] ──recv──▶ Receiver (auto-extend on)
//	                                          ↓ (process for 6s, past vt)
//	                                          Ack
//	Client ──poll──▶ [no redelivery]
func TestIntegration_SQS_AutoExtendKeepsMessageInvisible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("id5")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "4",
	})

	sendToSQS(t, client, queueURL, `{"autoextend":"test"}`, nil)

	ep := sqslocal.Endpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	autoExtend := true
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       1,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 4,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var deliveryCount int
	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		deliveryCount++
		if deliveryCount > 1 {
			t.Fatal("unexpected redelivery despite auto-extend")
		}
		time.Sleep(6 * time.Second) // ESSENTIAL: exceed 4s visibility timeout to prove auto-extend prevents redelivery
		if err := del.Ack(ctx); err != nil {
			t.Fatalf("Ack after auto-extend: %v", err)
		}
		cancel()
		return nil
	})

	if deliveryCount != 1 {
		t.Fatalf("expected exactly 1 delivery (auto-extend prevented redelivery), got %d", deliveryCount)
	}
}

// ---------------------------------------------------------------------------
// Helpers specific to delivery lifecycle tests
// ---------------------------------------------------------------------------

func pollSQSNoDelete(t *testing.T, client *awssqs.Client, queueURL string, timeout time.Duration) []string {
	t.Helper()
	var bodies []string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			time.Sleep(200 * time.Millisecond) // OTHER: backoff on transient SQS error
			continue
		}
		for _, msg := range out.Messages {
			bodies = append(bodies, *msg.Body)
		}
		if len(bodies) > 0 {
			return bodies
		}
	}
	return bodies
}
