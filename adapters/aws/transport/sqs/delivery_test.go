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
)

func TestDelivery_Envelope(t *testing.T) {
	env := &domain.Envelope{ID: "msg-1", Subject: "test"}
	mock := &mockSQSClient{}
	d := newDelivery(context.Background(), env, mock, "q-url", "rh-1", 30, false, nil)

	if d.Envelope() != env {
		t.Fatal("Envelope() should return the original envelope")
	}
}

func TestDelivery_Ack_DeletesMessage(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "https://q", "receipt-1", 30, false, nil)

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

func TestDelivery_Ack_Error(t *testing.T) {
	mock := &mockSQSClient{
		DeleteMessageFn: func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			return nil, errors.New("AccessDenied")
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil)

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

func TestDelivery_Retry_ZeroDelay(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil)

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

func TestDelivery_Retry_WithDelay(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil)

	if err := d.Retry(context.Background(), 10*time.Second, nil); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	if mock.ChangeVisibilityCalls[0].VisibilityTimeout != 10 {
		t.Fatalf("expected visibility 10, got %d", mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	}
}

func TestDelivery_Extend(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil)

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

func TestDelivery_Extend_ClampsMax(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, false, nil)

	until := time.Now().Add(48 * time.Hour)
	if err := d.Extend(context.Background(), until); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	if mock.ChangeVisibilityCalls[0].VisibilityTimeout != 43200 {
		t.Fatalf("expected max clamped to 43200, got %d", mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	}
}

func TestDelivery_AutoExtend_CallsChangeVisibility(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	// Use a very short visibility timeout (2s) so auto-extend fires at 1s.
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil)

	// Wait for at least one auto-extend to fire.
	time.Sleep(1500 * time.Millisecond)

	d.stop()
	time.Sleep(100 * time.Millisecond)

	count := extendCount.Load()
	if count < 1 {
		t.Fatalf("expected at least 1 auto-extend call, got %d", count)
	}
}

func TestDelivery_AutoExtend_StopsOnAck(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil)

	// Ack immediately to stop the auto-extend goroutine.
	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// Wait past the auto-extend interval.
	time.Sleep(1500 * time.Millisecond)

	count := extendCount.Load()
	if count > 0 {
		t.Fatalf("auto-extend should not fire after Ack, got %d calls", count)
	}
}

func TestDelivery_AutoExtend_StopsOnRetry(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}

	// The Retry call itself also calls ChangeMessageVisibility, so reset count.
	beforeCount := extendCount.Load()

	time.Sleep(1500 * time.Millisecond)

	afterCount := extendCount.Load()
	if afterCount > beforeCount {
		t.Fatalf("auto-extend should not fire after Retry, got %d additional calls", afterCount-beforeCount)
	}
}

func TestDelivery_NoAutoExtend(t *testing.T) {
	var extendCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, false, nil)

	time.Sleep(1500 * time.Millisecond)
	d.stop()

	if extendCount.Load() > 0 {
		t.Fatal("auto-extend should not fire when disabled")
	}
}

func TestDelivery_AutoExtend_StopsOnError(t *testing.T) {
	var callCount atomic.Int32
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			callCount.Add(1)
			return nil, errors.New("network error")
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	_ = newDelivery(context.Background(), env, mock, "q", "rh", 2, true, nil)

	// Wait for the auto-extend goroutine to fire and fail.
	time.Sleep(1500 * time.Millisecond)

	count := callCount.Load()
	if count != 1 {
		t.Fatalf("expected exactly 1 auto-extend attempt before stopping on error, got %d", count)
	}
}

func TestDelivery_AutoExtend_UsesCorrectTimeout(t *testing.T) {
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			if in.VisibilityTimeout != 10 {
				t.Errorf("auto-extend should use full timeout 10, got %d", in.VisibilityTimeout)
			}
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 10, true, nil)

	// Interval is 5s (50% of 10), wait a bit more than that.
	time.Sleep(5500 * time.Millisecond)
	d.stop()
}

func TestDelivery_MultipleStopsAreSafe(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, true, nil)

	d.stop()
	d.stop()
	d.stop()

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack after multiple stops should succeed: %v", err)
	}
}

func TestNewDelivery_WithQueueURLAndHandle(t *testing.T) {
	mock := &mockSQSClient{}
	env := &domain.Envelope{ID: "msg-1"}
	d := newDelivery(context.Background(), env, mock, "https://sqs.us-east-1.amazonaws.com/123/my-queue", "handle-abc", 30, false, nil)

	_ = d.Ack(context.Background())

	if *mock.DeleteCalls[0].QueueUrl != "https://sqs.us-east-1.amazonaws.com/123/my-queue" {
		t.Fatal("queue URL not passed correctly")
	}
	if *mock.DeleteCalls[0].ReceiptHandle != "handle-abc" {
		t.Fatal("receipt handle not passed correctly")
	}
}

func TestDelivery_Ack_StopsAutoExtendThenDeletes(t *testing.T) {
	callOrder := make([]string, 0, 3)
	mock := &mockSQSClient{
		DeleteMessageFn: func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			callOrder = append(callOrder, "delete")
			return &awssqs.DeleteMessageOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-ack-order"}
	d := newDelivery(context.Background(), env, mock, "q", "rh", 30, true, nil)

	_ = d.Ack(context.Background())

	if len(callOrder) == 0 || callOrder[0] != "delete" {
		t.Fatal("Ack should call DeleteMessage")
	}

	mock.mu.Lock()
	deleteCount := len(mock.DeleteCalls)
	mock.mu.Unlock()

	if deleteCount != 1 {
		t.Fatalf("expected exactly 1 delete, got %d", deleteCount)
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
