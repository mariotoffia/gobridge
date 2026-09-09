package sqs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestBugM6_AutoExtend_TickerResetsAfterVisibilityChange verifies that
// when Extend() changes the stored visibilityTimeout, the auto-extend
// loop adjusts its tick interval accordingly.
//
// Visibility starts at 2s (the cadence floor is 1s), then changes to 20s
// (cadence 20s/3). Clock advances must follow completed tick handling:
// the SDK call occurs BEFORE Reset, while MetricSQSAutoExtends occurs AFTER
// it. Advancing on the call count alone can queue an old-cadence tick which
// the fake clock correctly preserves through Reset.
func TestBugM6_AutoExtend_TickerResetsAfterVisibilityChange(t *testing.T) {
	var extendCount atomic.Int32

	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			extendCount.Add(1)
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}

	rec := &ports.RecordingExporter{}
	fake := clocktest.NewAt(time.Unix(0, 0))
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "visibility-change", CreatedAt: fake.Now()})

	d := newDelivery(
		t.Context(), env, mock, "q", "rh", 2, true, nil, nil, rec, fake,
	)
	t.Cleanup(func() {
		d.stopAutoExtend()
		d.cleanupContext()
		wait.Until(t, time.Second, "auto-extend ticker stopped", func() bool { return fake.TickerCount() == 0 })
	})

	// Wait for the auto-extend goroutine to register its ticker with
	// the fake clock before we advance time — otherwise Advance runs
	// while there is no ticker to fire and the first tick is lost.
	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Wait through the first handler's clock read/window update before
	// calling Extend or advancing the fake clock again.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first auto-extend tick handled", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 1
	})

	// Change visibility to 20s via Extend -- this itself issues one
	// ChangeMessageVisibility synchronously and updates the stored
	// visibilityTimeout atomically.
	if err := d.Extend(t.Context(), fake.Now().Add(20*time.Second)); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}
	if got := d.visibilityTimeout.Load(); got != 20 {
		t.Fatalf("stored visibility = %ds, want 20s", got)
	}
	if got := extendCount.Load(); got != 2 {
		t.Fatalf("calls after one tick and explicit Extend = %d, want 2", got)
	}

	// Advance 1s more at the old 1s interval — this tick is the one
	// that lets autoExtendLoop observe the new visibilityTimeout and
	// call ticker.Reset(20s/3). Observe the completed reset before advancing.
	const newInterval = 20 * time.Second / 3
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "reset tick fully handled", func() bool {
		periods := fake.TickerPeriods()
		return fake.TickerResets() == 1 &&
			len(periods) == 1 && periods[0] == newInterval &&
			len(rec.FindEntries(MetricSQSAutoExtends)) == 2
	})
	countAfterReset := extendCount.Load()
	if countAfterReset != 3 {
		t.Fatalf("calls after the reset tick = %d, want 3", countAfterReset)
	}

	// Three seconds is shorter than the new interval, so no call may fire.
	fake.Advance(3 * time.Second)

	// Retain the original zero-extra-call assertion and observation window.
	stable := wait.StableFor(t, extendCount.Load, 50*time.Millisecond, 500*time.Millisecond)
	newCalls := stable - countAfterReset
	if newCalls != 0 {
		t.Fatalf("expected 0 auto-extend calls in 3s after visibility "+
			"change to 20s (20s/3 interval), got %d new calls", newCalls)
	}

	// Also prove the loop continues at the new cadence, rather than merely
	// going quiet or stopping after the reset.
	fake.Advance(newInterval - 3*time.Second)
	wait.Until(t, time.Second, "next tick at the new cadence", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 3
	})
	if got := extendCount.Load(); got != 4 {
		t.Fatalf("calls at the new cadence = %d, want 4", got)
	}
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

	rec := &ports.RecordingExporter{}
	fake := clocktest.NewAt(time.Unix(0, 0))
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "unchanged-visibility", CreatedAt: fake.Now()})
	d := newDelivery(
		t.Context(), env, mock, "q", "rh", 2, true, nil, nil, rec, fake,
	)
	t.Cleanup(func() {
		d.stopAutoExtend()
		d.cleanupContext()
		wait.Until(t, time.Second, "auto-extend ticker stopped", func() bool { return fake.TickerCount() == 0 })
	})

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first tick handled", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 1
	})
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "second tick handled", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 2
	})

	if got := extendCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 auto-extend calls at 1s interval, got %d", got)
	}
	if got := fake.TickerResets(); got != 0 {
		t.Fatalf("unchanged visibility reset the ticker %d times", got)
	}
}
