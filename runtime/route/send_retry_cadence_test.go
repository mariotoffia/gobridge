package route

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSendRetry_WaitNeverDropsBelowTheFloor pins the floor under the wait
// between two in-process sends. Validation accepts a 1 ms backoff that never
// grows, and a destination may hint a 1 ns RetryAfter; waited verbatim, either
// turns one held delivery into tens of thousands of sends inside a 60 s budget,
// or into a CPU-bound loop once jitter rounds the wait to zero. Every wait is
// raised to 100 ms, and it is raised BEFORE the budget is asked whether it can
// cover the wait: with a 150 ms budget the second send lands at 100 ms, and the
// next floored wait no longer fits, so the delivery stops after two sends.
//
// Mutation check: wait the computed delay unraised and this fails — the second
// send happens before 100 ms, and the loop keeps sending until the test gives
// up on it.
func TestSendRetry_WaitNeverDropsBelowTheFloor(t *testing.T) {
	const floor = 100 * time.Millisecond
	tiny := routing.BackoffPolicy{
		InitialInterval: time.Millisecond,
		MaxInterval:     time.Millisecond,
		Multiplier:      1,
		JitterFactor:    routing.JitterDisabled,
	}
	cases := []struct {
		name string
		err  error
	}{
		{name: "a 1 ms backoff", err: shared.ErrUnavailable},
		{name: "a 1 ns RetryAfter hint", err: shared.ErrUnavailable.WithRetryAfter(time.Nanosecond)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &flakySender{err: tc.err, failures: -1}
			f := newSendRetryFixtureWithBackoff(150*time.Millisecond, tiny, sender)
			del := &stubDelivery{env: countLessEnv("send-retry-floor")}

			done := f.handle(context.Background(), del)
			f.awaitRetryWait(t, 1)

			// Advance fires every timer that falls due before it returns, so a
			// wait shorter than the floor has already ended here.
			f.clk.Advance(floor - time.Millisecond)
			if n := f.clk.TimerCount(); n != 1 {
				t.Fatalf("armed timers 1 ms before the floor = %d, want 1: the retry wait ended early", n)
			}
			if got := sender.sends.Load(); got != 1 {
				t.Fatalf("sends 1 ms before the floor = %d, want 1", got)
			}

			f.clk.Advance(time.Millisecond)
			if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}
			if got := sender.sends.Load(); got != 2 {
				t.Fatalf("sends = %d, want 2: one at 0 and one at the 100 ms floor", got)
			}
			if !del.retried || del.acked {
				t.Fatalf("settlement acked=%v retried=%v, want a source redelivery", del.acked, del.retried)
			}
			f.assertRetryMetrics(t, 1, 1)
		})
	}
}
