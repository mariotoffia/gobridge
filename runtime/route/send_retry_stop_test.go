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
