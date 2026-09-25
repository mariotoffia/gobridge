package route

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// The in-process send retry wraps every direct_hold send. These pin what that
// costs: first the healthy path, where the loop must add nothing but one clock
// read, then a delivery that fails twice and succeeds on its third send.

// benchSendRetryRunner builds a direct_hold route with the default send retry
// budget, on the real clock except that the floored retry wait fires at once.
func benchSendRetryRunner(sender ports.Sender) *RouteRunner {
	return NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "bench-send-retry",
		Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:  sender,
		DLQ:     dlq.New(&recordingDLQStore{}),
		Metrics: &ports.NoopExporter{},
		Clock:   floorSkippingClock{clock.System},
	})
}

// floorSkippingClock is the real clock, except that a timer armed for exactly
// minSendRetryWait fires at once. Every retry wait in these benchmarks is raised
// to that floor, so each op would otherwise sleep 100 ms per retry and measure
// the sleep; the wedge-ceiling timer every send arms is a real timer as always.
type floorSkippingClock struct{ clock.Clock }

func (c floorSkippingClock) NewTimer(d time.Duration) clock.Timer {
	if d == minSendRetryWait {
		d = 0
	}
	return c.Clock.NewTimer(d)
}

// BenchmarkSendDirectHold_FirstSendSucceeds measures a held send that succeeds
// at once, with the retry budget enabled.
func BenchmarkSendDirectHold_FirstSendSucceeds(b *testing.B) {
	r := benchSendRetryRunner(stubSender{})
	env := generatedIDEnv("bench-send-ok")
	plan := routing.DispatchPlan{BindingID: "b1", Address: "addr"}
	b.ReportAllocs()
	for b.Loop() {
		del := &stubDelivery{env: env}
		if err := r.sendDirectHold(context.Background(), del, env, plan); err != nil {
			b.Fatal(err)
		}
		if !del.acked {
			b.Fatal("the send succeeded but the delivery was not acked")
		}
	}
}

// BenchmarkSendDirectHold_RetriesThenSucceeds measures a held send that fails
// twice and succeeds on the third send. The failures carry a 1 ns RetryAfter
// hint, which the loop raises to minSendRetryWait; the bench clock fires that
// wait at once, so the measurement is the retry machinery itself.
func BenchmarkSendDirectHold_RetriesThenSucceeds(b *testing.B) {
	hinted := shared.ErrUnavailable.WithRetryAfter(time.Nanosecond)
	var sends atomic.Int64
	r := benchSendRetryRunner(benchSendFunc(func() error {
		if sends.Add(1)%3 != 0 {
			return hinted
		}
		return nil
	}))
	env := generatedIDEnv("bench-send-retried")
	plan := routing.DispatchPlan{BindingID: "b1", Address: "addr"}
	b.ReportAllocs()
	for b.Loop() {
		del := &stubDelivery{env: env}
		_ = r.sendDirectHold(context.Background(), del, env, plan)
		if !del.acked {
			b.Fatal("the third send succeeded but the delivery was not acked")
		}
	}
}

// benchSendFunc adapts a function to ports.Sender.
type benchSendFunc func() error

func (f benchSendFunc) Send(context.Context, ports.OutboundMessage) error { return f() }
