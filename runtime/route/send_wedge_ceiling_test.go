package route

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSendWedgeCeiling_ZeroSendTimeoutIsUnbounded proves the documented
// contract of a zero send timeout: NO wedge bound, await completion.
//
// sendWedgeCeiling returns 0 for "no bound", but boundedSend armed
// clk.NewTimer(0) with it — a timer that fires immediately, so the very first
// send would be classified as hung, the route wedged and the delivery timed out
// into a duplicate retry. The runner's WithDefaults currently fills SendTimeout
// before this can be reached, which is exactly why the inverted contract went
// unnoticed; the guard belongs where the contract is stated.
func TestSendWedgeCeiling_ZeroSendTimeoutIsUnbounded(t *testing.T) {
	sender := &releaseSender{entered: make(chan struct{}), release: make(chan struct{}), err: shared.ErrUnavailable}
	defer sender.unblock()

	clk := clocktest.New()
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "zero-ceiling",
		Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:  sender,
		Metrics: &ports.RecordingExporter{},
		Clock:   clk,
	})
	// WithDefaults fills SendTimeout, so disable it the only way the contract
	// allows to reach the unbounded branch.
	r.policy.SendTimeout = 0
	if got := r.sendWedgeCeiling(); got != 0 {
		t.Fatalf("sendWedgeCeiling with SendTimeout disabled = %v, want 0 (no bound)", got)
	}

	const binding = "b1"
	msg := ports.OutboundMessage{Envelope: countLessEnv("zero-ceiling"), Address: "addr"}

	errc := make(chan error, 1)
	go func() { errc <- r.boundedSend(context.Background(), sender, msg, binding) }()
	<-sender.entered

	// No bound means no armed timer at all: an immediately-firing timer would
	// wedge the route on its first send. Sample until the count is STABLE — a
	// bare read could observe the dispatcher goroutine before it reached the
	// select and pass for the wrong reason.
	if n := wait.StableFor(t, clk.TimerCount, 100*time.Millisecond, 2*time.Second); n != 0 {
		t.Fatalf("armed timers = %d, want 0: a zero ceiling means NO bound, not a timer that "+
			"fires at once and wedges every send", n)
	}
	// Advancing the clock past any plausible ceiling must not settle the send.
	clk.Advance(time.Hour)
	select {
	case err := <-errc:
		t.Fatalf("send completed while still parked (err=%v); an unbounded ceiling must await the sender", err)
	default:
	}

	sender.unblock()
	if err := <-errc; !shared.IsRecoverableError(err) {
		t.Fatalf("send error = %v, want the sender's own recoverable error", err)
	}
	if r.isWedged() {
		t.Fatal("an unbounded send that RETURNED must never wedge the route")
	}
}
