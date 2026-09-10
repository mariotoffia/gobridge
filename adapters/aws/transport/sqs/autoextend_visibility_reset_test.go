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

// Force the CI test's interleaving: the third SDK call is observable, but its
// handler has not reset the ticker. Advancing by 3s here queues an old-cadence
// tick. Reset must preserve that already-delivered tick, not discard it to make
// an incorrectly synchronized zero-extra-call assertion pass.
func TestAutoExtendVisibilityResetPreservesAlreadyDeliveredTick(t *testing.T) {
	var calls atomic.Int32
	resetCall := make(chan struct{})
	release := make(chan struct{})
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(ctx context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			if calls.Add(1) == 3 {
				close(resetCall)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
	}
	fake := clocktest.NewAt(time.Unix(0, 0))
	rec := &ports.RecordingExporter{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "buffered-visibility-tick", CreatedAt: fake.Now()})
	d := newDelivery(t.Context(), env, mock, "q", "rh", 2, true, nil, nil, rec, fake)
	t.Cleanup(func() {
		d.stopAutoExtend()
		d.cleanupContext()
		wait.Until(t, time.Second, "auto-extend ticker stopped", func() bool { return fake.TickerCount() == 0 })
	})
	wait.Until(t, time.Second, "ticker registered", func() bool { return fake.TickerCount() == 1 })

	fake.Advance(time.Second)
	wait.Until(t, time.Second, "first tick handled", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 1
	})
	if err := d.Extend(t.Context(), fake.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	fake.Advance(time.Second)
	wait.RequireClosed(t, resetCall, time.Second)
	if calls.Load() != 3 || fake.TickerResets() != 0 {
		t.Fatal("the third call must precede the cadence reset")
	}

	// The handler is blocked, so this advance deterministically precedes
	// Reset and delivers one buffered tick from the old 1s cadence.
	fake.Advance(3 * time.Second)
	close(release)
	wait.Until(t, time.Second, "reset and buffered tick handled", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtends)) == 3
	})
	if got := calls.Load(); got != 4 {
		t.Fatalf("expected one already-delivered tick after the reset call, total calls = %d, want 4", got)
	}
	periods := fake.TickerPeriods()
	if fake.TickerResets() != 1 || len(periods) != 1 || periods[0] != 20*time.Second/3 {
		t.Fatalf("ticker did not adopt the new cadence: periods=%v, resets=%d", periods, fake.TickerResets())
	}
}
