package sqs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG M6: SQS auto-extend ticker interval never updates
//
// When Extend() updates visibilityTimeout, the auto-extend loop continued
// ticking at the original interval because the ticker was never Reset().
// The fix recomputes the interval after each successful extend and calls
// ticker.Reset if it changed.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugM6_AutoExtend_TickerResetsAfterVisibilityChange verifies that
// when Extend() changes the stored visibilityTimeout, the auto-extend
// loop adjusts its tick interval accordingly.
//
// Strategy: start with a 2s visibility (tick at 1s), then call Extend
// to change visibility to 20s (tick at 10s). If the ticker is NOT reset,
// the auto-extend fires again at 1s after the change. If it IS reset,
// it should not fire for ~10s. We wait 3s after the change and check
// the call count did not increase.
func TestBugM6_AutoExtend_TickerResetsAfterVisibilityChange(t *testing.T) {
	var extendCount atomic.Int32

	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	rec := &ports.RecordingExporter{}
	env := &domain.Envelope{ID: "msg-m6"}

	// Start with visibility=2s so auto-extend fires every 1s.
	d := newDelivery(
		context.Background(), env, mock, "q", "rh", 2, true, nil, nil, rec,
	)

	// Wait for the first auto-extend tick (~1s).
	time.Sleep(1500 * time.Millisecond)

	countBefore := extendCount.Load()
	if countBefore < 1 {
		t.Fatalf("expected at least 1 auto-extend call before Extend, got %d", countBefore)
	}

	// Change visibility to 20s via Extend -- the auto-extend loop should
	// now tick every 10s instead of 1s.
	if err := d.Extend(context.Background(), time.Now().Add(20*time.Second)); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	// Record the count right after Extend (Extend itself calls
	// ChangeMessageVisibility once).
	countAfterExtend := extendCount.Load()

	// Wait 3s. If the ticker was NOT reset, it would still fire at 1s
	// intervals, adding ~3 more calls. If reset to 10s, zero new calls.
	time.Sleep(3 * time.Second)

	countAfterWait := extendCount.Load()
	newCalls := countAfterWait - countAfterExtend

	// Allow at most 1 extra call (timing margin), but not 3.
	if newCalls > 1 {
		t.Fatalf("expected 0-1 auto-extend calls in 3s after visibility "+
			"change to 20s (10s interval), got %d new calls", newCalls)
	}

	d.stop()
}

// TestBugM6_AutoExtend_SameTimeout_NoReset verifies that when the
// visibility timeout has not changed, the ticker is not needlessly reset.
// This is a no-regression sanity check.
func TestBugM6_AutoExtend_SameTimeout_NoReset(t *testing.T) {
	var extendCount atomic.Int32

	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-m6b"}
	d := newDelivery(
		context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil,
	)

	// Wait for at least 2 ticks (~2s with 1s interval).
	time.Sleep(2500 * time.Millisecond)
	d.stop()

	count := extendCount.Load()
	if count < 2 {
		t.Fatalf("expected at least 2 auto-extend calls at 1s interval, got %d", count)
	}
}
