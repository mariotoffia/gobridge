package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies Run receives SQS messages and invokes the handler with mapped envelope and headers.
func TestReceiver_RunEmitsDeliveries(t *testing.T) {
	callCount := 0
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-001"),
						ReceiptHandle: aws.String("rh-001"),
						Body:          aws.String(`{"key":"value"}`),
						MessageAttributes: map[string]sqstypes.MessageAttributeValue{
							"custom": {DataType: aws.String("String"), StringValue: aws.String("val")},
						},
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
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
	if env.ID() != "msg-001" {
		t.Fatalf("expected ID msg-001, got %s", env.ID())
	}
	if string(env.Payload()) != `{"key":"value"}` {
		t.Fatalf("unexpected payload: %s", string(env.Payload()))
	}
	if env.Headers()["custom"] != "val" {
		t.Fatal("custom header not mapped")
	}
}

// Verifies reserved bridge headers from message attributes are stripped before delivery.
func TestReceiver_RunStripsReservedHeaders(t *testing.T) {
	callCount := 0
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-inj"),
						ReceiptHandle: aws.String("rh-inj"),
						Body:          aws.String("body"),
						MessageAttributes: map[string]sqstypes.MessageAttributeValue{
							messaging.HeaderCorrelationID: {DataType: aws.String("String"), StringValue: aws.String("injected")},
							messaging.HeaderRouteID:       {DataType: aws.String("String"), StringValue: aws.String("injected")},
							"safe-header":                 {DataType: aws.String("String"), StringValue: aws.String("ok")},
						},
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
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

	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; ok {
		t.Fatal("x-bridge.correlation-id should be stripped at ingress")
	}
	if _, ok := env.Headers()[messaging.HeaderRouteID]; ok {
		t.Fatal("x-bridge.route-id should be stripped at ingress")
	}
	if env.Headers()["safe-header"] != "ok" {
		t.Fatal("safe-header should be preserved")
	}
}

// Verifies SNS-wrapped bodies are unwrapped into subject, payload, and topic metadata.
func TestReceiver_RunSNSUnwrap(t *testing.T) {
	snsMsg := map[string]string{
		"TopicArn": "arn:aws:sns:us-west-1:123:my-topic",
		"Subject":  "Test Subject",
		"Message":  `{"inner":"data"}`,
	}
	snsBody, _ := json.Marshal(snsMsg)

	callCount := 0
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-sns"),
						ReceiptHandle: aws.String("rh-sns"),
						Body:          aws.String(string(snsBody)),
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		SNSUnwrap:         true,
		Client:            mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var env *messaging.Envelope
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		env = del.Envelope()
		cancel()
		return nil
	})

	if env.Subject() != "Test Subject" {
		t.Fatalf("expected subject from SNS, got %q", env.Subject())
	}
	if string(env.Payload()) != `{"inner":"data"}` {
		t.Fatalf("expected unwrapped SNS message body, got %q", string(env.Payload()))
	}
	if env.Headers()["sns.topic_arn"] != "arn:aws:sns:us-west-1:123:my-topic" {
		t.Fatal("SNS topic ARN should be in headers")
	}
}

// Verifies Run retries ReceiveMessage after a transient error and eventually delivers.
func TestReceiver_RunRetriesOnReceiveError(t *testing.T) {
	var callCount int
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount == 1 {
				return nil, errors.New("temporary network error")
			}
			if callCount == 2 {
				return &awssqs.ReceiveMessageOutput{
					Messages: []sqstypes.Message{
						{
							MessageId:     aws.String("msg-retry"),
							ReceiptHandle: aws.String("rh-retry"),
							Body:          aws.String("retried body"),
						},
					},
				}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
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

// Verifies Run returns the handler error and stops when the emit callback fails.
func TestReceiver_RunStopsOnEmitError(t *testing.T) {
	callCount := 0
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-stop"),
						ReceiptHandle: aws.String("rh-stop"),
						Body:          aws.String("stop me"),
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
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

// Conformance (ports.Receiver emit-error contract): when emit returns an
// error the delivery is NOT settled — the SQS message is neither deleted
// (acked) nor visibility-changed — so it returns to the queue for
// redelivery after the visibility timeout expires.
func TestReceiver_EmitError_DeliveryNotDeleted(t *testing.T) {
	mock := &mockSQSClient{
		ReceiveMessageFn: func(_ context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-emit-err"),
						ReceiptHandle: aws.String("rh-emit-err"),
						Body:          aws.String("body"),
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("pipeline rejected delivery")
	runErr := recv.Run(context.Background(), func(_ context.Context, _ ports.Delivery) error {
		return sentinel
	})

	if !errors.Is(runErr, sentinel) {
		t.Fatalf("Run should surface the emit error, got %v", runErr)
	}
	if len(mock.DeleteCalls) != 0 {
		t.Fatalf("delivery must not be deleted/acked on emit error, got %d delete calls", len(mock.DeleteCalls))
	}
	if len(mock.ChangeVisibilityCalls) != 0 {
		t.Fatalf("delivery must not be settled via visibility change on emit error, got %d calls", len(mock.ChangeVisibilityCalls))
	}
}

// Verifies NewReceiver rejects configuration without a queue URL.
func TestReceiver_Validate_RequiresQueue(t *testing.T) {
	_, err := NewReceiver(ReceiverConfig{}, nil)
	if err == nil {
		t.Fatal("expected error when no queue specified")
	}
}

// Verifies Ack on a delivery deletes using the message's receipt handle.
func TestReceiver_DeliveryAck_DeletesCorrectHandle(t *testing.T) {
	callCount := 0
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{
				Messages: []sqstypes.Message{
					{
						MessageId:     aws.String("msg-del"),
						ReceiptHandle: aws.String("rh-del-specific"),
						Body:          aws.String("body"),
					},
				},
			}, nil
		},
	}

	noAutoExtend := false
	recv, _ := NewReceiver(ReceiverConfig{
		QueueURL:          "https://my-queue",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = recv.Run(ctx, func(c context.Context, del ports.Delivery) error {
		if err := del.Ack(c); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		cancel()
		return nil
	})

	if len(mock.DeleteCalls) != 1 {
		t.Fatalf("expected 1 delete, got %d", len(mock.DeleteCalls))
	}
	if *mock.DeleteCalls[0].ReceiptHandle != "rh-del-specific" {
		t.Fatal("wrong receipt handle deleted")
	}
}
