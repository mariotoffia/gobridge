package sqs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// autoExtendTicksHandled counts the auto-extend ticks the loop has carried all
// the way through, either outcome.
//
// It is a synchronisation point, not an assertion. Both branches emit their
// counter AFTER reading the clock for the window check, so a test that has seen
// tick i counted knows the loop is no longer reading the instant it is about to
// change — which a wait on the mock call cannot know, because the call is made
// before that read.
func autoExtendTicksHandled(rec *ports.RecordingExporter) int {
	return len(rec.FindEntries(MetricSQSAutoExtends)) + len(rec.FindEntries(MetricSQSAutoExtendFailures))
}

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
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	// rec is the ORDERING BARRIER between the Advance calls, not a metrics
	// assertion — the same one the deadline-lapse test below relies on, and
	// for the same reason: the CMV call is recorded BEFORE the loop reads
	// clk.Now() for its window check, so advancing on the call alone can move
	// the clock to t=2s under an iteration that fired at t=1s. At this
	// visibility the deadline sits exactly one tick ahead, so the loop would
	// read a lapsed window and cancel processing on its first failure.
	rec := &ports.RecordingExporter{}
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-1", 2, true, nil, nil, rec, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	// The budget is wall-clock while the tick itself is fake-clock: Advance
	// only releases the auto-extend goroutine, which then has to be SCHEDULED.
	// Under a parallel -race integration run that scheduling can slip past a
	// 1s budget, so these use the repo's 2s default.
	wait.Until(t, 2*time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance to trigger first tick (will fail with "transient").
	fake.Advance(1 * time.Second)
	wait.Until(t, 2*time.Second, "first tick fully handled", func() bool {
		return autoExtendTicksHandled(rec) >= 1
	})

	// SYNC: advance to trigger second tick (will succeed).
	fake.Advance(1 * time.Second)
	wait.Until(t, 2*time.Second, "second tick fully handled", func() bool {
		return autoExtendTicksHandled(rec) >= 2
	})

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
//
// It drives twice the ceiling in failures, which is what makes the reset the
// subject. A loop that never reset the counter would still handle the first few
// ticks — its third NON-consecutive failure is where it would give up — so a
// shorter run cannot tell the two apart.
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
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e3", Payload: []byte("z"), CreatedAt: time.Now()})
	fake := clocktest.New()
	// rec is the ORDERING BARRIER between iterations. See the note on the
	// transient-retry test above: waiting on the CMV call alone lets the next
	// Advance move the clock under an iteration that has not yet read it, and
	// at this visibility one tick of overshoot is the whole window — the loop
	// then cancels processing on its first failure and never ticks again.
	rec := &ports.RecordingExporter{}
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-3", 2, true, nil, nil, rec, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, 2*time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance far enough that a loop without the reset would have hit the
	// ceiling and returned, so its missing ticks fail the wait below.
	const ticks = autoExtendMaxFailures*2 + 2
	for i := 1; i <= ticks; i++ {
		fake.Advance(1 * time.Second)
		wait.Until(t, 2*time.Second, "tick handled (a loop that never reset its failure counter gives up here)", func() bool {
			return autoExtendTicksHandled(rec) >= i
		})
	}

	total := callCount.Load()
	if total < int32(ticks) {
		t.Fatalf("the loop handled %d of %d interleaved ticks: it gave up while its failures were "+
			"still non-consecutive, so the success reset of the consecutive-failure counter is gone",
			total, ticks)
	}
}

// TestAutoExtendStopsAfterMaxFailuresS15 verifies the loop gives up and
// cancels processing after autoExtendMaxFailures consecutive failures.
//
// Uses visibility=30 (interval 10s, then a 5s retry after the second
// failure) so that three consecutive failures all land strictly before
// the visibility window lapses at 30s — isolating the consecutive-failure
// ceiling from the deadline-lapse cancel path (Finding 5). A processing
// cancel func records that the loop actually gave up.
func TestAutoExtendStopsAfterMaxFailuresS15(t *testing.T) {
	t.Parallel()

	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		return nil, errors.New("always fail")
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Payload: []byte("y"), CreatedAt: time.Now()})
	fake := clocktest.New()
	var cancelled atomic.Bool
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-2", 30, true,
		func() { cancelled.Store(true) }, nil, nil, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, 2*time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Cadence: tick at vis/3 = 10s; after the 2nd failure the remaining
	// window has halved so the loop Resets the ticker to a 5s retry.
	// Advance along that exact schedule so three consecutive failures fire
	// before the 30s deadline.
	//
	// wantPeriod is the ORDERING BARRIER. Waiting only on the CMV call
	// races the loop's ticker.Reset, which happens later in the same
	// iteration: advancing 5s before the Reset lands re-arms nextTick to
	// now+5s instead of firing at it, the third tick never arrives and the
	// test hangs. Waiting for the period to change proves the Reset is in.
	for _, step := range []struct {
		advance    time.Duration
		wantCalls  int
		wantPeriod time.Duration // ticker cadence once this tick is fully handled
	}{
		{10 * time.Second, 1, 10 * time.Second}, // retry == cadence → no Reset
		{10 * time.Second, 2, 5 * time.Second},  // remaining window halved → Reset(5s)
		{5 * time.Second, 3, 0},                 // ceiling reached → loop returns
	} {
		fake.Advance(step.advance)
		wait.Until(t, 2*time.Second, "failure tick", func() bool {
			mock.mu.Lock()
			n := len(mock.ChangeVisibilityCalls)
			mock.mu.Unlock()
			return n >= step.wantCalls
		})
		if step.wantPeriod == 0 {
			continue
		}
		wait.Until(t, 2*time.Second, "ticker re-armed at the retry cadence", func() bool {
			periods := fake.TickerPeriods()
			return len(periods) == 1 && periods[0] == step.wantPeriod
		})
	}

	wait.Until(t, 2*time.Second, "processing cancelled", func() bool {
		return cancelled.Load()
	})

	mock.mu.Lock()
	n := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()
	if n < autoExtendMaxFailures {
		t.Fatalf("ChangeMessageVisibility calls: want >= %d, got %d", autoExtendMaxFailures, n)
	}
}

// TestAutoExtendCancelsOnDeadlineLapseAtMinVisibilityS15 exercises the
// deadline-driven cancel (windowLapsed) branch in isolation — the PRIMARY
// value of Finding 5 — which only LEADS at the minimum visibility (vis=2,
// interval floored to 1s). With vis=2 the window lapses at t=2s while the
// consecutive-failure ceiling (autoExtendMaxFailures=3) has not yet been
// reached, so processing must be cancelled after EXACTLY 2 failed CMV
// calls with consecutiveFailures (2) strictly below the ceiling — proving
// the deadline branch, not the ceiling, triggered.
//
// Fails-without: a loop that only cancelled on the failure ceiling would
// keep extending a message whose visibility had already lapsed (letting it
// resurface to another consumer), never firing the cancel here.
func TestAutoExtendCancelsOnDeadlineLapseAtMinVisibilityS15(t *testing.T) {
	t.Parallel()

	mock := &mockSQSClient{}
	mock.ChangeMessageVisibilityFn = func(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
		return nil, errors.New("always fail")
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e-minvis", Payload: []byte("y"), CreatedAt: time.Now()})
	fake := clocktest.New()
	var cancelled atomic.Bool
	// rec is the ORDERING BARRIER between the two Advance calls, not a
	// metrics assertion. The loop records the CMV call, THEN reads
	// clk.Now() for the window check, THEN counts the failure
	// (acl_delivery.go: ChangeMessageVisibility → clk.Now() → Counter).
	// Advancing on the CMV call alone races that middle step: the clock
	// could reach t=2s before tick 1's window check reads it, lapsing the
	// window after ONE call and cancelling for the wrong reason. Waiting
	// for the counter proves clk.Now() was already read at t=1s.
	rec := &ports.RecordingExporter{}
	// vis=2 → interval floored to 1s, deadline at t=2s.
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-minvis", 2, true,
		func() { cancelled.Store(true) }, nil, rec, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, 2*time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Tick 1 at t=1s: fails, cf=1, window not yet lapsed (1s < 2s) → retry.
	fake.Advance(1 * time.Second)
	wait.Until(t, 2*time.Second, "first failure counted at t=1s", func() bool {
		return len(rec.FindEntries(MetricSQSAutoExtendFailures)) >= 1
	})

	// Tick 2 at t=2s: fails, cf=2 (< ceiling 3), window lapsed (2s !< 2s)
	// → deadline-driven cancel.
	fake.Advance(1 * time.Second)
	wait.Until(t, 2*time.Second, "processing cancelled by deadline", func() bool {
		return cancelled.Load()
	})

	mock.mu.Lock()
	n := len(mock.ChangeVisibilityCalls)
	mock.mu.Unlock()
	if n != 2 {
		t.Fatalf("ChangeMessageVisibility calls: want exactly 2 (deadline branch, "+
			"cf=2 < ceiling %d), got %d", autoExtendMaxFailures, n)
	}
}
