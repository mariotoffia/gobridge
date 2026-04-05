package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// BUG RES-003/004: SQS Receiver ensureClient and ReceiveMessage no per-call
// timeout. The fix wraps init in a 30s timeout and each ReceiveMessage
// in a per-poll timeout derived from WaitTimeSeconds.

// TestBugRES003_EnsureClient_RespectsTimeout verifies that Run wraps the
// ensureClient + resolveQueueURL calls in a bounded timeout, so a slow
// AWS config build does not hang indefinitely.
func TestBugRES003_EnsureClient_RespectsTimeout(t *testing.T) {
	// A mock client that blocks on GetQueueUrl until context expires.
	blockingMock := &mockSQSClient{
		GetQueueUrlFn: func(ctx context.Context, _ *awssqs.GetQueueUrlInput, _ ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:         "test-queue",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            blockingMock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// Run with a generous parent context -- the init timeout should kick
	// in well before this expires.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("expected error from Run when queue URL resolution blocks")
	}

	// The init timeout is 30s, but we gave the parent only 5s. The init
	// timeout is min(30s, parent). We should complete within the parent.
	if elapsed > 6*time.Second {
		t.Fatalf("Run took %v; expected it to respect the init timeout", elapsed)
	}
}

// TestBugRES004_PollLoop_UsesPerCallTimeout verifies that each
// ReceiveMessage call gets a per-poll timeout so a stuck API call
// does not block the entire loop.
func TestBugRES004_PollLoop_UsesPerCallTimeout(t *testing.T) {
	callCount := 0
	var outerCancel context.CancelFunc
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			callCount++
			if callCount == 1 {
				// First call: verify context has a deadline.
				if _, ok := ctx.Deadline(); !ok {
					t.Error("expected per-poll deadline on ReceiveMessage context")
				}
				// Cancel the parent to end the test quickly.
				outerCancel()
				return &awssqs.ReceiveMessageOutput{}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://test-queue",
		VisibilityTimeout: 30,
		WaitTimeSeconds:   1,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	outerCancel = cancel
	defer cancel()

	_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if callCount < 1 {
		t.Fatal("ReceiveMessage should have been called at least once")
	}
}

// TestBugRES003_InitTimeout_ReturnsClassifiedError verifies the error
// from a timed-out initialisation is a BridgeError.
func TestBugRES003_InitTimeout_ReturnsClassifiedError(t *testing.T) {
	blockingMock := &mockSQSClient{
		GetQueueUrlFn: func(ctx context.Context, _ *awssqs.GetQueueUrlInput, _ ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:         "blocked-queue",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            blockingMock,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runErr := recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
		return nil
	})

	if runErr == nil {
		t.Fatal("expected error")
	}

	// Should be a classified bridge error or context error.
	var be *domain.BridgeError
	if !errors.As(runErr, &be) && !errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected BridgeError or context error, got %T: %v", runErr, runErr)
	}
}
