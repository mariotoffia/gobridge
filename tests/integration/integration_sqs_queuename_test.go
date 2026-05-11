package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"testing"
	"time"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS QueueName resolution integration tests with ElasticMQ
//
// Validates that Sender and Receiver configured with QueueName (instead of
// QueueURL) lazily resolve the queue URL and function correctly end-to-end.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────────────┐
// │ Test │ Description                                                  │
// ├──────┼──────────────────────────────────────────────────────────────┤
// │ QN1  │ Sender resolves QueueName → URL on first Send               │
// │ QN2  │ Receiver resolves QueueName → URL on Run                    │
// │ QN3  │ Full Sender→SQS→Receiver round-trip with headers & subject  │
// │ QN4  │ Batch send 15 messages, receive all via Receiver            │
// └──────┴──────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_SQS_Sender_QueueNameResolution validates that a Sender
// configured with QueueName (not QueueURL) lazily resolves the queue URL
// on the first Send call.
//
// Scenario:
//
//	Sender(QueueName) ──resolve──▶ GetQueueUrl
//	                   ──send──▶ [SQS Queue] ◀──poll── Client
func TestIntegration_SQS_Sender_QueueNameResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := sqslocal.Client(t)
	queueName := sqslocal.UniqueQueue("qn1")
	queueURL := sqslocal.CreateQueue(t, client, queueName)

	ep := sqslocal.Endpoint(t)
	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueName: queueName,
		Endpoint:  ep,
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qn1-msg-1",
		Subject: "qn1-subject",
		Payload: []byte("resolved-by-name"),
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	bodies := pollSQS(t, client, queueURL, 1, 10*time.Second)
	if len(bodies) != 1 {
		t.Fatalf("expected 1 message, got %d", len(bodies))
	}
	if bodies[0] != "resolved-by-name" {
		t.Fatalf("body mismatch: got %q, want %q", bodies[0], "resolved-by-name")
	}
}

// TestIntegration_SQS_Receiver_QueueNameResolution validates that a Receiver
// configured with QueueName resolves the queue URL when Run is called.
//
// Scenario:
//
//	Client ──send──▶ [SQS Queue]
//	Receiver(QueueName) ──resolve──▶ GetQueueUrl
//	                     ──recv──▶ callback
func TestIntegration_SQS_Receiver_QueueNameResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := sqslocal.Client(t)
	queueName := sqslocal.UniqueQueue("qn2")
	queueURL := sqslocal.CreateQueue(t, client, queueName)

	sendToSQS(t, client, queueURL, `{"resolved":"by-name"}`, nil)

	ep := sqslocal.Endpoint(t)
	autoExtend := false
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueName:         queueName,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var received []ports.Delivery
	err = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		received = append(received, del)
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if err != nil && ctx.Err() == nil {
		t.Fatalf("receiver.Run: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(received))
	}

	env := received[0].Envelope()
	if string(env.Payload) != `{"resolved":"by-name"}` {
		t.Fatalf("payload mismatch: got %q", string(env.Payload))
	}
}

// TestIntegration_SQS_SenderReceiver_FullRoundTrip validates the complete
// Sender → SQS → Receiver pipeline including Subject, string and numeric
// headers, and payload integrity.
//
// Scenario:
//
//	Sender ──send(subject, headers)──▶ [SQS Queue] ──recv──▶ Receiver
//	                                                          ↓
//	                                                Verify subject, headers, payload
func TestIntegration_SQS_SenderReceiver_FullRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := sqslocal.Client(t)
	queueName := sqslocal.UniqueQueue("qn3")
	sqslocal.CreateQueue(t, client, queueName)

	ep := sqslocal.Endpoint(t)

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueName: queueName,
		Endpoint:  ep,
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qn3-roundtrip",
		Subject: "orders.created",
		Payload: []byte(`{"order_id":"12345","amount":99.95}`),
		Headers: map[string]any{
			"X-Correlation-ID": "corr-abc-123",
			"X-Priority":       42,
			"X-Source":         "integration-test",
		},
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	autoExtend := false
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueName:         queueName,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got *messaging.Envelope
	err = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		got = del.Envelope()
		_ = del.Ack(ctx)
		cancel()
		return nil
	})

	if err != nil && ctx.Err() == nil {
		t.Fatalf("receiver.Run: %v", err)
	}
	if got == nil {
		t.Fatal("no envelope received")
	}

	if string(got.Payload) != `{"order_id":"12345","amount":99.95}` {
		t.Fatalf("payload mismatch: got %q", string(got.Payload))
	}

	if got.Subject() != "orders.created" {
		t.Fatalf("subject mismatch: got %q, want %q", got.Subject(), "orders.created")
	}

	checkHdr := func(key, want string) {
		t.Helper()
		v, ok := got.Headers()[key]
		if !ok {
			t.Fatalf("header %q missing; headers=%v", key, got.Headers())
		}
		var s string
		switch val := v.(type) {
		case string:
			s = val
		default:
			t.Fatalf("header %q unexpected type %T", key, v)
		}
		if s != want {
			t.Fatalf("header %q: got %q, want %q", key, s, want)
		}
	}

	checkHdr("X-Correlation-ID", "corr-abc-123")
	checkHdr("X-Source", "integration-test")

	// Numeric headers are serialised as Number attributes; on receive
	// they come back as string values.
	numVal, ok := got.Headers()["X-Priority"]
	if !ok {
		t.Fatalf("header X-Priority missing; headers=%v", got.Headers())
	}
	switch v := numVal.(type) {
	case string:
		if v != "42" {
			t.Fatalf("X-Priority: got %q, want %q", v, "42")
		}
	default:
		t.Fatalf("X-Priority unexpected type %T: %v", numVal, numVal)
	}
}

// TestIntegration_SQS_Sender_BatchThenReceive validates that batch-sent
// messages can all be received by the Receiver. Sends 15 messages via
// SendBatch (split into batches of 10 + 5), then receives all 15.
//
// Scenario:
//
//	Sender ──SendBatch(15)──▶ [SQS Queue] ──recv──▶ Receiver (15 deliveries)
//	                           batch 1: [0..9]
//	                           batch 2: [10..14]
func TestIntegration_SQS_Sender_BatchThenReceive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := sqslocal.Client(t)
	queueName := sqslocal.UniqueQueue("qn4")
	sqslocal.CreateQueue(t, client, queueName)

	ep := sqslocal.Endpoint(t)

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueName: queueName,
		Endpoint:  ep,
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	const total = 15
	envs := make([]*messaging.Envelope, total)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("qn4-batch-%d", i),
			Subject: "batch-subject",
			Payload: []byte(fmt.Sprintf("batch-body-%d", i)),
			Headers: map[string]any{
				"X-Seq": strconv.Itoa(i),
			},
		})
	}

	sent, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != total {
		t.Fatalf("expected %d sent, got %d", total, sent)
	}

	autoExtend := false
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueName:         queueName,
		Endpoint:          ep,
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 10,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	received := make(map[string]bool, total)
	err = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		env := del.Envelope()
		received[string(env.Payload)] = true
		_ = del.Ack(ctx)
		if len(received) >= total {
			cancel()
		}
		return nil
	})

	if err != nil && ctx.Err() == nil {
		t.Fatalf("receiver.Run: %v", err)
	}
	if len(received) != total {
		t.Fatalf("expected %d messages, got %d", total, len(received))
	}

	for i := 0; i < total; i++ {
		key := fmt.Sprintf("batch-body-%d", i)
		if !received[key] {
			t.Fatalf("missing message: %q", key)
		}
	}
}
