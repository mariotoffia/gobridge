package sqs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
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
// The test uses a clocktest.Fake so the auto-extend ticker fires
// deterministically when time is advanced, with no wall-clock sleeps.
//
//  1. Start with visibility=2s → auto-extend ticks every 1s.
//  2. Advance 1s → the ticker fires and exactly one auto-extend call is
//     observed on the mock.
//  3. Call Extend(20s) — the call itself performs one synchronous
//     ChangeMessageVisibility. The auto-extend loop must reset its
//     ticker to 10s on the next tick.
//  4. Advance 1s to drive one more tick so the loop observes the new
//     visibilityTimeout and resets the ticker to 10s.
//  5. Advance 3s. If the ticker was NOT reset, 3 extra calls would fire.
//     With the fix, zero extra calls fire within 3s of the reset (the
//     next tick is now 10s away in fake time).
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
	fake := clocktest.New()

	d := newDelivery(
		context.Background(), env, mock, "q", "rh", 2, true, nil, nil, rec, fake,
	)

	// Wait for the auto-extend goroutine to register its ticker with
	// the fake clock before we advance time — otherwise Advance runs
	// while there is no ticker to fire and the first tick is lost.
	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Advance 1s → ticker fires once; autoExtendLoop issues one
	// ChangeMessageVisibility call. Wait for the async goroutine to
	// deliver the call to the mock.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first auto-extend tick", func() bool {
		return extendCount.Load() >= 1
	})

	// Change visibility to 20s via Extend -- this itself issues one
	// ChangeMessageVisibility synchronously and updates the stored
	// visibilityTimeout atomically.
	if err := d.Extend(context.Background(), time.Now().Add(20*time.Second)); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}
	wait.Until(t, time.Second, "extend sync call observed", func() bool {
		return extendCount.Load() >= 2
	})

	// Advance 1s more at the old 1s interval — this tick is the one
	// that lets autoExtendLoop observe the new visibilityTimeout and
	// call ticker.Reset(10s).
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "reset tick observed", func() bool {
		return extendCount.Load() >= 3
	})
	countAfterReset := extendCount.Load()

	// Advance 3 more seconds. At the new 10s interval, zero ticks fire.
	// At the old (unreset) 1s interval, 3 ticks would fire.
	fake.Advance(3 * time.Second)

	// Small settle window: if any extra tick were going to be delivered
	// it would be visible to the mock within a handful of scheduler
	// hops. Use wait.StableFor to assert the extend counter stays at
	// its post-reset value for a brief window (i.e. no late tick leaks
	// through).
	stable := wait.StableFor(t, extendCount.Load, 50*time.Millisecond, 500*time.Millisecond)
	newCalls := stable - countAfterReset
	if newCalls != 0 {
		t.Fatalf("expected 0 auto-extend calls in 3s after visibility "+
			"change to 20s (10s interval), got %d new calls", newCalls)
	}

	d.stopAutoExtend()
	d.cleanupContext()
}

// TestBugM6_AutoExtend_SameTimeout_NoReset verifies that when the
// visibility timeout has not changed, the ticker keeps firing at the
// original interval (i.e. nothing regresses the tick cadence). Uses a
// fake clock so the test is deterministic and instant.
func TestBugM6_AutoExtend_SameTimeout_NoReset(t *testing.T) {
	var extendCount atomic.Int32

	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	env := &domain.Envelope{ID: "msg-m6b"}
	fake := clocktest.New()
	d := newDelivery(
		context.Background(), env, mock, "q", "rh", 2, true, nil, nil, nil, fake,
	)

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first tick", func() bool {
		return extendCount.Load() >= 1
	})
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "second tick", func() bool {
		return extendCount.Load() >= 2
	})

	d.stopAutoExtend()
	d.cleanupContext()

	if got := extendCount.Load(); got < 2 {
		t.Fatalf("expected at least 2 auto-extend calls at 1s interval, got %d", got)
	}
}
