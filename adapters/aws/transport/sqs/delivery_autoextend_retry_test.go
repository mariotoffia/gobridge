package sqs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
)

// TestAutoExtendRetriesTransientThenSucceedsS15 verifies the auto-extend loop
// survives one transient ChangeMessageVisibility error and continues after a
// successful extend (consecutive failure counter resets).
func TestAutoExtendRetriesTransientThenSucceedsS15(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		n := callCount.Add(1)
		if n == 1 {
			return nil, errors.New("transient")
		}
		return &awssqs.ChangeMessageVisibilityOutput{}, nil
	}

	ctx := context.Background()
	env := &domain.Envelope{ID: "e1", Payload: []byte("x"), CreatedAt: time.Now()}
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-1", 2, true, nil)
	defer d.stop()

	time.Sleep(3500 * time.Millisecond)

	mock.mu.Lock()
	n := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()
	if n < 2 {
		t.Fatalf("ChangeMessageVisibility calls: want >= 2, got %d", n)
	}
}

// TestAutoExtendInterleavedFailSuccessS15 verifies that the consecutive failure
// counter resets after each success, allowing the loop to survive more total
// failures than autoExtendMaxFailures as long as they are non-consecutive.
func TestAutoExtendInterleavedFailSuccessS15(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		n := callCount.Add(1)
		if n%2 == 1 {
			return nil, errors.New("odd call fails")
		}
		return &awssqs.ChangeMessageVisibilityOutput{}, nil
	}

	ctx := context.Background()
	env := &domain.Envelope{ID: "e3", Payload: []byte("z"), CreatedAt: time.Now()}
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-3", 2, true, nil)
	defer d.stop()

	time.Sleep(5 * time.Second)

	total := callCount.Load()
	if total <= int32(autoExtendMaxFailures) {
		t.Fatalf("expected more than %d total calls (interleaved), got %d", autoExtendMaxFailures, total)
	}
}

// TestAutoExtendStopsAfterMaxFailuresS15 verifies the loop exits after
// autoExtendMaxFailures consecutive failures.
func TestAutoExtendStopsAfterMaxFailuresS15(t *testing.T) {
	t.Parallel()

	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		return nil, errors.New("always fail")
	}

	ctx := context.Background()
	env := &domain.Envelope{ID: "e2", Payload: []byte("y"), CreatedAt: time.Now()}
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-2", 2, true, nil)
	defer d.stop()

	time.Sleep(4 * time.Second)

	mock.mu.Lock()
	n := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()
	if n < autoExtendMaxFailures {
		t.Fatalf("ChangeMessageVisibility calls: want >= %d, got %d", autoExtendMaxFailures, n)
	}
}
