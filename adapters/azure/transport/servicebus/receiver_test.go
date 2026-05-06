package servicebus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// verifies Receiver.Run emits a delivery with payload, ID, and queue subject from a mock client.
func TestReceiver_RunEmitsDeliveries(t *testing.T) {
	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-001",
					Body:      []byte(`{"key":"value"}`),
					ApplicationProperties: map[string]any{
						"custom": "val",
					},
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var deliveries []ports.Delivery
	var mu sync.Mutex

	err = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		mu.Lock()
		deliveries = append(deliveries, del)
		mu.Unlock()
		cancel()
		return nil
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}

	env := deliveries[0].Envelope()
	if env.ID != "msg-001" {
		t.Fatalf("expected ID msg-001, got %s", env.ID)
	}
	if string(env.Payload) != `{"key":"value"}` {
		t.Fatalf("unexpected payload: %s", string(env.Payload))
	}
	if env.Subject != "test-queue" {
		t.Fatalf("expected Subject test-queue, got %s", env.Subject)
	}
}

// verifies ingress strips bridge correlation headers while preserving other application properties.
func TestReceiver_RunStripsReservedHeaders(t *testing.T) {
	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-inj",
					Body:      []byte("body"),
					ApplicationProperties: map[string]any{
						messaging.HeaderCorrelationID: "injected",
						"safe-header":                 "ok",
					},
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var env *messaging.Envelope
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		env = del.Envelope()
		cancel()
		return nil
	})

	if _, ok := env.Headers[messaging.HeaderCorrelationID]; ok {
		t.Fatal("x-bridge.correlation-id should be stripped at ingress")
	}
	if env.Headers["safe-header"] != "ok" {
		t.Fatal("safe-header should be preserved")
	}
}

// verifies Run retries receive after a transient error and eventually delivers a message.
func TestReceiver_RunRetriesOnReceiveError(t *testing.T) {
	var callCount int
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount == 1 {
				return nil, errors.New("temporary network error")
			}
			if callCount == 2 {
				return []*azservicebus.ReceivedMessage{
					{
						MessageID: "msg-retry",
						Body:      []byte("retried body"),
					},
				}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotDelivery bool
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		gotDelivery = true
		cancel()
		return nil
	})

	if !gotDelivery {
		t.Fatal("should have received a delivery after the retry")
	}
	if callCount < 2 {
		t.Fatal("should have retried after first error")
	}
}

// verifies Run returns the handler error and stops when the emit callback fails.
func TestReceiver_RunStopsOnEmitError(t *testing.T) {
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-stop",
					Body:      []byte("stop me"),
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("stop now")
	err = recv.Run(context.Background(), func(_ context.Context, _ ports.Delivery) error {
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// verifies NewReceiver rejects configuration with neither queue nor topic.
func TestReceiver_Validate_RequiresQueue(t *testing.T) {
	_, err := NewReceiver(ReceiverConfig{}, nil)
	if err == nil {
		t.Fatal("expected error when no queue or topic specified")
	}
}

// verifies envelope Subject defaults to the configured topic name for topic receivers.
func TestReceiver_SubjectFromTopic(t *testing.T) {
	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-topic",
					Body:      []byte("topic body"),
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		TopicName:        "my-topic",
		SubscriptionName: "my-sub",
		AutoExtend:       boolPtr(false),
		Client:           mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var env *messaging.Envelope
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		env = del.Envelope()
		cancel()
		return nil
	})

	if env.Subject != "my-topic" {
		t.Fatalf("expected Subject my-topic, got %s", env.Subject)
	}
}

// verifies the message Subject property overrides the queue-based default subject.
func TestReceiver_SubjectOverrideFromMessage(t *testing.T) {
	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-subj",
					Body:      []byte("subject body"),
					Subject:   strPtr("override-subject"),
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var env *messaging.Envelope
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		env = del.Envelope()
		cancel()
		return nil
	})

	if env.Subject != "override-subject" {
		t.Fatalf("expected Subject override-subject, got %s", env.Subject)
	}
}

// verifies Ack on a Run-emitted delivery completes the correct underlying message.
func TestReceiver_DeliveryAck_CompletesCorrectMessage(t *testing.T) {
	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{
				{
					MessageID: "msg-ack",
					Body:      []byte("ack body"),
				},
			}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "test-queue",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = recv.Run(ctx, func(c context.Context, del ports.Delivery) error {
		if err := del.Ack(c); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		cancel()
		return nil
	})

	if len(mock.CompleteCalls) != 1 {
		t.Fatalf("expected 1 complete call, got %d", len(mock.CompleteCalls))
	}
	if mock.CompleteCalls[0].MessageID != "msg-ack" {
		t.Fatalf("wrong message completed: got %s", mock.CompleteCalls[0].MessageID)
	}
}
