package route

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// What stops the in-process send retry loop while it is WAITING, rather than
// right after a send. The loop spends most of its budget parked on a backoff
// timer, and both the route's terminal wedge and the budget itself have to hold
// across that park — the state a held delivery is in can change while it sleeps,
// and the delay it is sleeping for is a number the destination chose.

// TestSendRetry_BudgetBoundaryIsInclusive pins the exact edge of the budget
// stop condition. A retry is refused when the time already spent PLUS the wait
// would EXCEED the budget, so a budget that exactly covers the first wait still
// gets its retry, and one nanosecond short of it does not.
//
// Both ends of the loop have to measure it the same way — the check before the
// wait and the re-measure after it — or a route whose budget is set to exactly
// its first backoff interval silently never retries at all, while the operator
// reads the two numbers as fitting.
//
// Mutation check: declare the budget spent at elapsed >= budget after the wait
// and this fails — the exactly-covered budget performs no retry.
func TestSendRetry_BudgetBoundaryIsInclusive(t *testing.T) {
	// The fixture's backoff is 1 s doubling to 30 s, un-jittered, so the first
	// wait is exactly one second.
	const firstWait = time.Second
	cases := []struct {
		name          string
		budget        time.Duration
		wantSends     int32
		wantRetries   int
		wantExhausted int
	}{
		{name: "a budget that exactly covers the first wait retries",
			budget: firstWait, wantSends: 2, wantRetries: 1},
		{name: "one nanosecond short of it does not",
			budget: firstWait - 1, wantSends: 1, wantExhausted: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &flakySender{err: shared.ErrUnavailable, failures: 1}
			f := newSendRetryFixture(tc.budget, sender)
			del := &stubDelivery{env: countLessEnv("send-retry-boundary")}

			done := f.handle(context.Background(), del)
			if tc.wantSends > 1 {
				f.awaitRetryWait(t, 1)
				f.clk.Advance(firstWait)
			}
			if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}

			if got := sender.sends.Load(); got != tc.wantSends {
				t.Fatalf("sends = %d, want %d", got, tc.wantSends)
			}
			// The retry cures the failure, so the exactly-covered budget acks;
			// the budget one nanosecond short never sends again, and its
			// count-less message goes back to the source.
			if tc.wantSends > 1 && (!del.acked || del.retried) {
				t.Errorf("settlement acked=%v retried=%v, want the ack the successful retry earns", del.acked, del.retried)
			}
			if tc.wantSends == 1 && (!del.retried || del.acked) {
				t.Errorf("settlement acked=%v retried=%v, want a source redelivery", del.acked, del.retried)
			}
			f.assertRetryMetrics(t, tc.wantRetries, tc.wantExhausted)
		})
	}
}

// TestSendRetry_WedgeDuringTheWaitStopsTheNextSend pins the wedge as a stop
// condition across the wait, not only immediately after a send. Another
// delivery in flight can latch the route's terminal wedge while this one backs
// off; a wedged route refuses new deliveries and escalates, so the delivery
// that wakes from its wait must not start another physical send into it. It
// takes the same outcome a wedge always gave: a transient failure its source
// redelivers.
//
// Mutation check: re-check only the delivery context when the wait ends and
// this fails — the woken delivery counts a retry and sends again after the
// terminal wedge has latched.
func TestSendRetry_WedgeDuringTheWaitStopsTheNextSend(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: -1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: countLessEnv("send-retry-wedge-during-wait")}

	done := f.handle(context.Background(), del)
	f.awaitRetryWait(t, 1)
	// Another delivery's hung send latches the terminal wedge while this one is
	// parked on its retry timer.
	_ = f.r.wedge(errSenderWedged)
	f.clk.Advance(time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1: a wedged route must not send again", got)
	}
	if !del.retried || del.acked {
		t.Fatalf("settlement acked=%v retried=%v, want a source redelivery", del.acked, del.retried)
	}
	if attempts, _ := f.hook.snapshot(); len(attempts) != 1 {
		t.Fatalf("OnAttempt = %d, want 1", len(attempts))
	}
	if got := f.store.writes.Load(); got != 0 {
		t.Fatalf("DLQ writes = %d, want 0", got)
	}
	f.assertRetryMetrics(t, 0, 0)
}

// TestSendRetry_LateWakePastTheBudgetDoesNotSendAgain pins the budget as a
// wall-clock bound, not merely a bound on what was SCHEDULED. A timer
// guarantees a minimum delay and nothing more: scheduler pressure or a GC pause
// can resume a parked delivery long after the budget the delay was measured
// against. Starting another send there would put the last physical send past
// the budget and let it run a further send_timeout — the very hold both route
// validator rules are sized on, so the source window they protect would be
// overrun by a route the validator accepted.
//
// Mutation check: re-check only the retry predicate when the wait ends and this
// fails — the late delivery counts a retry and sends again past its budget.
func TestSendRetry_LateWakePastTheBudgetDoesNotSendAgain(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: -1}
	f := newSendRetryFixture(10*time.Second, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-late-wake")}

	done := f.handle(context.Background(), del)
	f.awaitRetryWait(t, 1)
	// One step past both the 1 s delay and the whole 10 s budget: the timer has
	// long since fallen due, which is exactly what a late wake looks like to the
	// loop.
	f.clk.Advance(30 * time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1: a send may not start after the budget is spent", got)
	}
	// The same terminal outcome a spent budget always produced: an uncountable
	// message is poisoned, its source settled.
	if !del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want the terminal poison ack", del.acked, del.retried)
	}
	if got := f.store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1", got)
	}
	if got := countTaggedCounter(f.rec, shared.MetricDLQEntries, shared.TagKeyCategory, "unstable_identity"); got != 1 {
		t.Fatalf("DLQEntries{category=unstable_identity} = %d, want 1", got)
	}
	if attempts, _ := f.hook.snapshot(); len(attempts) != 1 {
		t.Fatalf("OnAttempt = %d, want 1", len(attempts))
	}
	f.assertRetryMetrics(t, 0, 1)
}

// TestSendRetry_AbsurdRetryAfterHintExhaustsTheBudget pins the budget check
// against a delay the DESTINATION chose. A RetryAfter hint is authoritative and
// used verbatim — never capped, never jittered — so a sender may hand back one
// near the largest duration there is. Added to the time the delivery has
// already spent, such a hint wraps the sum negative, which reads as "fits
// inside the budget": a one-minute route would arm a centuries-long timer and
// hold its source on it. The budget is declared spent instead, and the delivery
// takes the decision it always took.
//
// Mutation check: compare elapsed + delay against the budget and this fails —
// the delivery never returns, because it is parked on a timer that outlives the
// test.
func TestSendRetry_AbsurdRetryAfterHintExhaustsTheBudget(t *testing.T) {
	sender := &releaseSender{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     shared.ErrUnavailable.WithRetryAfter(time.Duration(math.MaxInt64)),
	}
	defer sender.unblock()
	f := newSendRetryFixture(10*time.Second, sender)
	del := &stubDelivery{env: countLessEnv("send-retry-absurd-hint")}

	done := f.handle(context.Background(), del)
	wait.RequireClosed(t, sender.entered, 5*time.Second)
	waitTimerCount(t, f.clk, 1) // the wedge ceiling of the in-flight send
	// Spend a second of the budget INSIDE the send, so the elapsed time the
	// hint is measured against is non-zero. The wedge ceiling is 35 s with the
	// default send timeout, well clear of this.
	f.clk.Advance(time.Second)
	sender.unblock()

	// The clock never moves again: a retry wait that was armed at all parks the
	// delivery here for longer than this test, or any deployment, will live.
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	attempts, _ := f.hook.snapshot()
	if len(attempts) != 1 {
		t.Fatalf("physical sends = %d, want 1: a hint past the budget is not waited out", len(attempts))
	}
	if n := f.clk.TimerCount(); n != 0 {
		t.Fatalf("armed timers = %d, want 0: no wait may be armed for a hint the budget cannot cover", n)
	}
	if !del.retried || del.acked {
		t.Fatalf("settlement acked=%v retried=%v, want a source redelivery", del.acked, del.retried)
	}
	f.assertRetryMetrics(t, 0, 1)
}
