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
	"github.com/mariotoffia/gobridge/testutil/wait"
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
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("x"), CreatedAt: time.Now()})
	fake := clocktest.New()
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-1", 2, true, nil, nil, nil, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance to trigger first tick (will fail with "transient").
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first tick", func() bool {
		return callCount.Load() >= 1
	})

	// SYNC: advance to trigger second tick (will succeed).
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "second tick", func() bool {
		return callCount.Load() >= 2
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
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-3", 2, true, nil, nil, nil, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// SYNC: advance multiple ticks to get > autoExtendMaxFailures total calls.
	for i := 1; i <= autoExtendMaxFailures+2; i++ {
		fake.Advance(1 * time.Second)
		wait.Until(t, time.Second, "tick fired", func() bool {
			return callCount.Load() >= int32(i)
		})
	}

	total := callCount.Load()
	if total <= int32(autoExtendMaxFailures) {
		t.Fatalf("expected more than %d total calls (interleaved), got %d", autoExtendMaxFailures, total)
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

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Cadence: tick at vis/3 = 10s; after the 2nd failure the loop resets
	// to a 5s retry (half the remaining window). Advance along that exact
	// schedule so three consecutive failures fire before the 30s deadline.
	for i, adv := range []time.Duration{10 * time.Second, 10 * time.Second, 5 * time.Second} {
		fake.Advance(adv)
		want := i + 1
		wait.Until(t, time.Second, "failure tick", func() bool {
			mock.mu.Lock()
			n := len(mock.ChangeVisibilityCalls)
			mock.mu.Unlock()
			return n >= want
		})
	}

	wait.Until(t, time.Second, "processing cancelled", func() bool {
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
	// vis=2 → interval floored to 1s, deadline at t=2s.
	d := newDelivery(ctx, env, mock, "https://test-queue", "receipt-minvis", 2, true,
		func() { cancelled.Store(true) }, nil, nil, fake)
	defer func() { d.stopAutoExtend(); d.cleanupContext() }()

	wait.Until(t, time.Second, "ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Tick 1 at t=1s: fails, cf=1, window not yet lapsed (1s < 2s) → retry.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "first failure tick", func() bool {
		mock.mu.Lock()
		n := len(mock.ChangeVisibilityCalls)
		mock.mu.Unlock()
		return n >= 1
	})

	// Tick 2 at t=2s: fails, cf=2 (< ceiling 3), window lapsed (2s !< 2s)
	// → deadline-driven cancel.
	fake.Advance(1 * time.Second)
	wait.Until(t, time.Second, "processing cancelled by deadline", func() bool {
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
