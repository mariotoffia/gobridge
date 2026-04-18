package sqs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Verifies Envelope returns the underlying domain envelope.
func TestDelivery_Envelope(t *testing.T) {
	env := &domain.Envelope{ID: "msg-1", Subject: "test"}
	mock := &mockSQSClient{}
	d := newDelivery(context.Background(), env, mock, "q-url", "rh-1", 30, false, nil, nil, nil, nil)

	if d.Envelope() != env {
		t.Fatal("Envelope() should return the original envelope")
	}
}

// Verifies Ack deletes the message using the correct queue URL and receipt handle.
func TestDelivery_Ack_DeletesMessage(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "https://q", "receipt-1", 30, false, nil, nil, nil, nil)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	if len(mock.DeleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteMessage call, got %d", len(mock.DeleteCalls))
	}
	if *mock.DeleteCalls[0].QueueUrl != "https://q" {
		t.Fatal("wrong queue URL in DeleteMessage")
	}
	if *mock.DeleteCalls[0].ReceiptHandle != "receipt-1" {
		t.Fatal("wrong receipt handle in DeleteMessage")
	}
}

// Verifies Ack maps SQS delete failures to a domain bridge error with NOT_AUTHORIZED when appropriate.
func TestDelivery_Ack_Error(t *testing.T) {
	mock := &mockSQSClient{
		DeleteMessageFn: func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			return nil, errors.New("AccessDenied")
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	err := d.Ack(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	be, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatal("expected BridgeError")
	}
	if be.Code != domain.ErrCodeNotAuthorized {
		t.Fatalf("expected NOT_AUTHORIZED, got %s", be.Code)
	}
}

// Verifies Retry with zero delay sets visibility timeout to zero for immediate redelivery.
func TestDelivery_Retry_ZeroDelay(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	if err := d.Retry(context.Background(), 0, errors.New("transient")); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	if len(mock.ChangeVisibilityCalls) != 1 {
		t.Fatalf("expected 1 ChangeMessageVisibility call, got %d", len(mock.ChangeVisibilityCalls))
	}
	if mock.ChangeVisibilityCalls[0].VisibilityTimeout != 0 {
		t.Fatalf("expected visibility 0 for immediate retry, got %d", mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	}
}

// Verifies Retry translates a delay into the expected visibility timeout seconds.
func TestDelivery_Retry_WithDelay(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	if err := d.Retry(context.Background(), 10*time.Second, nil); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	if len(mock.ChangeVisibilityCalls) == 0 {
		t.Fatal("expected at least 1 ChangeMessageVisibility call")
	}
	if mock.ChangeVisibilityCalls[0].VisibilityTimeout != 10 {
		t.Fatalf("expected visibility 10, got %d", mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	}
}

// Verifies Extend issues ChangeMessageVisibility with a timeout derived from the target time.
func TestDelivery_Extend(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	until := time.Now().Add(60 * time.Second)
	if err := d.Extend(context.Background(), until); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	if len(mock.ChangeVisibilityCalls) != 1 {
		t.Fatal("expected 1 ChangeMessageVisibility call")
	}
	vis := mock.ChangeVisibilityCalls[0].VisibilityTimeout
	if vis < 55 || vis > 65 {
		t.Fatalf("expected visibility ~60, got %d", vis)
	}
}

// Verifies Extend clamps visibility timeout to the SQS maximum (12 hours).
func TestDelivery_Extend_ClampsMax(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil, nil, nil, nil)

	until := time.Now().Add(48 * time.Hour)
	if err := d.Extend(context.Background(), until); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	if len(mock.ChangeVisibilityCalls) == 0 {
		t.Fatal("expected at least 1 ChangeMessageVisibility call")
	}
	if mock.ChangeVisibilityCalls[0].VisibilityTimeout != 43200 {
		t.Fatalf("expected max clamped to 43200, got %d", mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	}
}

// Verifies auto-extend periodically calls ChangeMessageVisibility while the delivery is active.
func TestDelivery_AutoExtend_CallsChangeVisibility(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	fake := clocktest.New()
	// Use a very short visibility timeout (2s) so auto-extend fires at 1s.
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil, fake)

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance fake clock to trigger auto-extend tick.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "auto-extend fired", func() bool {
		return extendCount.Load() >= 1
	})

	d.stopAutoExtend()
	d.cleanupContext()

	count := extendCount.Load()
	if count < 1 {
		t.Fatalf("expected at least 1 auto-extend call, got %d", count)
	}
}

// Verifies auto-extend stops after Ack so no further visibility changes occur.
func TestDelivery_AutoExtend_StopsOnAck(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil, nil)

	// Ack immediately to stop the auto-extend goroutine.
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// NEGATIVE: verify no further ChangeMessageVisibility after Ack
	<-time.After(200 * time.Millisecond)

	count := extendCount.Load()
	if count > 0 {
		t.Fatalf("auto-extend should not fire after Ack, got %d calls", count)
	}
}

// Verifies auto-extend does not continue after Retry beyond the Retry visibility call.
func TestDelivery_AutoExtend_StopsOnRetry(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil, nil)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	// The Retry call itself also calls ChangeMessageVisibility, so reset count.
	beforeCount := extendCount.Load()

	// NEGATIVE: verify no further ChangeMessageVisibility after Retry
	<-time.After(200 * time.Millisecond)

	afterCount := extendCount.Load()
	if afterCount > beforeCount {
		t.Fatalf("auto-extend should not fire after Retry, got %d additional calls", afterCount-beforeCount)
	}
}

// Verifies ChangeMessageVisibility is not invoked for background extension when auto-extend is disabled.
func TestDelivery_NoAutoExtend(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, false, nil, nil, nil, nil)

	// NEGATIVE: verify no ChangeMessageVisibility when auto-extend is disabled
	<-time.After(200 * time.Millisecond)
	d.stopAutoExtend()
	d.cleanupContext()

	if extendCount.Load() > 0 {
		t.Fatal("auto-extend should not fire when disabled")
	}
}

// Verifies auto-extend uses the full visibility timeout value in ChangeMessageVisibility.
func TestDelivery_AutoExtend_UsesCorrectTimeout(t *testing.T) {
	var callCount atomic.Int32
	var sawBad atomic.Bool
	var lastTimeout atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			callCount.Add(1)
			lastTimeout.Store(in.VisibilityTimeout)
			if in.VisibilityTimeout != 10 {
				sawBad.Store(true)
			}
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	fake := clocktest.New()
	d := newDelivery(context.Background(), env, mock, "q", "rh", 10, true, nil, nil, nil, fake)

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance fake clock past the 5s tick interval.
	fake.Advance(5 * time.Second)
	wait.Until(t, time.Second, "auto-extend fired", func() bool {
		return callCount.Load() >= 1
	})

	d.stopAutoExtend()
	d.cleanupContext()

	if callCount.Load() == 0 {
		t.Fatal("expected at least 1 auto-extend call")
	}
	if sawBad.Load() {
		t.Errorf("auto-extend should use full timeout 10, got %d", lastTimeout.Load())
	}
}

// Verifies calling stop multiple times leaves Ack working without panic or failure.
func TestDelivery_MultipleStopsAreSafe(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, true, nil, nil, nil, nil)

	d.stopAutoExtend()
	d.cleanupContext()
	d.stopAutoExtend()
	d.cleanupContext()
	d.stopAutoExtend()
	d.cleanupContext()

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack after multiple stops should succeed: %v", err)
	}
}

// Verifies delivery operations use the configured queue URL and receipt handle.
func TestNewDelivery_WithQueueURLAndHandle(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "https://sqs.us-west-1.amazonaws.com/123/my-queue", "handle-abc", 30, false, nil, nil, nil, nil)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	if len(mock.DeleteCalls) == 0 {
		t.Fatal("expected at least 1 DeleteMessage call")
	}
	if *mock.DeleteCalls[0].QueueUrl != "https://sqs.us-west-1.amazonaws.com/123/my-queue" {
		t.Fatal("queue URL not passed correctly")
	}
	if *mock.DeleteCalls[0].ReceiptHandle != "handle-abc" {
		t.Fatal("receipt handle not passed correctly")
	}
}

// Verifies Ack deletes the message after stopping auto-extend without extra spurious calls.
func TestDelivery_Ack_StopsAutoExtendThenDeletes(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		DeleteMessageFn: func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			return &awssqs.DeleteMessageOutput{}, nil
		},
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-ack-order"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil, nil)

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	mock.mu.Lock()
	deleteCount := len(mock.DeleteCalls)
	mock.mu.Unlock()
	if deleteCount != 1 {
		t.Fatalf("expected exactly 1 delete, got %d", deleteCount)
	}

	// NEGATIVE: verify no further ChangeMessageVisibility after Ack
	<-time.After(200 * time.Millisecond)
	if extendCount.Load() > 0 {
		t.Fatal("auto-extend should not fire after Ack")
	}
}

func init() {
	// Ensure *mockSQSClient satisfies sqsAPI at test compile time.
	var _ sqsAPI = (*mockSQSClient)(nil)

	// Ensure *sqsDelivery implements ports.Delivery at test compile time.
	var _ interface {
		Envelope() *domain.Envelope
		Ack(context.Context) error
		Retry(context.Context, time.Duration, error) error
		Extend(context.Context, time.Time) error
	} = (*sqsDelivery)(nil)

	// Suppress unused import warnings.
	_ = aws.String
}
