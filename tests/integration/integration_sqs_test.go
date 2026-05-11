package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS adapter integration tests with ElasticMQ
//
// Validates SQS Receiver and Sender behaviour against a real
// SQS-compatible endpoint (ElasticMQ in Docker).
//
// Summary:
// ┌──────┬─────────────────────────────────────────────────┐
// │ Test │ Description                                     │
// ├──────┼─────────────────────────────────────────────────┤
// │ IR1  │ Receiver emits deliveries from SQS messages     │
// │ IR2  │ Sender sends message visible via SQS client     │
// │ IR3  │ Sender writes to FIFO queue with ordering       │
// │ IR4  │ Receiver re-delivers after visibility timeout   │
// │ IR5  │ Receiver respects context cancellation           │
// └──────┴─────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_Receiver_ReceivesMessages validates that an SQS
// message sent via the raw client is emitted as a delivery by the Receiver.
//
// Scenario:
//
//	Client ──send──▶ [SQS Queue] ──receive──▶ Receiver ──emit──▶ callback
func TestIntegration_SQS_Receiver_ReceivesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	queueURL, sqsClient := setupSQSQueue(t, "ir1")
	receiver := newSQSReceiver(t, queueURL)

	sendToSQS(t, sqsClient, queueURL, `{"key":"value"}`, map[string]string{
		"X-Custom": "header-val",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var received []ports.Delivery
	err := receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		received = append(received, del)
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if err != nil && ctx.Err() == nil {
		t.Fatalf("receiver run error: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(received))
	}
	env := received[0].Envelope()
	if string(env.Payload) != `{"key":"value"}` {
		t.Fatalf("payload mismatch: got %q", string(env.Payload))
	}
}

// TestIntegration_SQS_Sender_SendsMessage validates that a message sent
// via the Sender appears in the SQS queue when polled with the raw client.
//
// Scenario:
//
//	Sender ──send──▶ [SQS Queue] ◀──poll── Client
func TestIntegration_SQS_Sender_SendsMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	queueURL, sqsClient := setupSQSQueue(t, "ir2")
	sender := newSQSSender(t, queueURL)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-ir2",
		Subject: "test-subject",
		Payload: []byte("hello from sender"),
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	bodies := pollSQS(t, sqsClient, queueURL, 1, 10*time.Second)
	if len(bodies) != 1 {
		t.Fatalf("expected 1 message, got %d", len(bodies))
	}
	if bodies[0] != "hello from sender" {
		t.Fatalf("body mismatch: got %q", bodies[0])
	}
}

// TestIntegration_SQS_Sender_FIFO validates FIFO queue behaviour by sending
// messages with a group ID and verifying they maintain order.
//
// Scenario:
//
//	Sender ──send(group)──▶ [SQS FIFO Queue] ◀──poll── Client
func TestIntegration_SQS_Sender_FIFO(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("ir3") + ".fifo"
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"FifoQueue":                 "true",
		"ContentBasedDeduplication": "true",
	})

	sender := newSQSSenderFIFO(t, queueURL, "test-group")

	for i := 0; i < 5; i++ {
		env := &messaging.Envelope{
			ID:      "msg-" + string(rune('a'+i)),
			Payload: []byte("order-" + string(rune('0'+i))),
		}
		if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
			t.Fatalf("sender.Send[%d]: %v", i, err)
		}
	}

	bodies := pollSQS(t, client, queueURL, 5, 10*time.Second)
	if len(bodies) < 5 {
		t.Fatalf("expected 5 messages, got %d", len(bodies))
	}

	for i, body := range bodies {
		expected := "order-" + string(rune('0'+i))
		if body != expected {
			t.Fatalf("message %d out of order: got %q, want %q", i, body, expected)
		}
	}
}

// TestIntegration_SQS_Receiver_VisibilityTimeout validates that a message
// becomes visible again after the visibility timeout expires without an Ack.
//
// Scenario:
//
//	Client ──send──▶ [Queue vt=2s] ──recv──▶ Receiver (no Ack)
//	... 2s ...
//	[Queue] ──redeliver──▶ Receiver
func TestIntegration_SQS_Receiver_VisibilityTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := sqslocal.Client(t)
	name := sqslocal.UniqueQueue("ir4")
	queueURL := sqslocal.CreateQueueWithAttrs(t, client, name, map[string]string{
		"VisibilityTimeout": "2",
	})
	receiver := newSQSReceiverWithVisibility(t, queueURL, 2)

	sendToSQS(t, client, queueURL, "redelivery-test", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var count int
	_ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		count++
		if count == 1 {
			return nil
		}
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if count < 2 {
		t.Fatalf("expected at least 2 deliveries (redelivery), got %d", count)
	}
}

// TestIntegration_SQS_Receiver_ContextCancel validates that the receiver
// stops cleanly when the context is cancelled.
func TestIntegration_SQS_Receiver_ContextCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	queueURL, _ := setupSQSQueue(t, "ir5")
	receiver := newSQSReceiver(t, queueURL)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- receiver.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			return nil
		})
	}()

	select {
	case <-receiver.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not start")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not stop after context cancellation")
	}
}
