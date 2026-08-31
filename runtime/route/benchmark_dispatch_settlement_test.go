package route

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// The cancellation guard runs at the entry of every recoverable dispatch
// branch, and the replay ledger is now read once more per retry (the backoff
// attempt). Both sit on the failure path of every delivery, so these pin what
// they cost — first the guard alone, then a whole delivery through the dispatch
// pipeline for the three outcomes a route actually produces.

// benchCancellingSender cancels the delivery in flight, the way a cooperative
// transport reports a bridge-initiated teardown. The cancel func is swapped per
// iteration; the benchmark drives it from one goroutine.
type benchCancellingSender struct{ cancel context.CancelFunc }

func (s *benchCancellingSender) Send(ctx context.Context, _ ports.OutboundMessage) error {
	s.cancel()
	return ctx.Err()
}

// benchDropPolicy is the shape the cancellation guard protects: a finite replay
// cap with on_permanent_failure=drop, where a wrong terminal decision loses the
// message.
func benchDropPolicy() routing.RoutePolicy {
	return routing.RoutePolicy{
		DeliveryMode:       routing.DeliveryDirectHold,
		MaxReplayAttempts:  5,
		SendTimeout:        time.Second,
		OnPermanentFailure: routing.FailureDrop,
	}.WithDefaults()
}

func benchRunner(b *testing.B, sender ports.Sender) *RouteRunner {
	b.Helper()
	return NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "bench-dispatch",
		Policy:   benchDropPolicy(),
		Sender:   sender,
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		DLQ:      dlq.New(&recordingDLQStore{}),
		Metrics:  &ports.NoopExporter{},
	})
}

// BenchmarkAbandonIfCancelled measures the guard itself on both arms: the live
// context (every ordinary failure pays this) and the cancelled one (paid once
// per in-flight message during a shutdown).
func BenchmarkAbandonIfCancelled(b *testing.B) {
	r := benchRunner(b, stubSender{err: shared.ErrUnavailable})
	env := generatedIDEnv("bench-guard")

	b.Run("live", func(b *testing.B) {
		b.ReportAllocs()
		var sink error
		for b.Loop() {
			sink = r.abandonIfCancelled(context.Background(), env, "send", shared.ErrUnavailable)
		}
		_ = sink
	})

	b.Run("cancelled", func(b *testing.B) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		b.ReportAllocs()
		var sink error
		for b.Loop() {
			sink = r.abandonIfCancelled(ctx, env, "send", context.Canceled)
		}
		_ = sink
	})
}

// BenchmarkDispatchSettlement walks a whole delivery through the dispatch
// pipeline for each terminal outcome a direct_hold route produces: a healthy
// send, a transient failure that retries (the ledger-backed backoff path), and
// an abandoned delivery under a cancelled context. The three together are the
// per-message cost an operator sees during normal traffic, a downstream outage,
// and a rolling restart.
func BenchmarkDispatchSettlement(b *testing.B) {
	b.Run("delivered", func(b *testing.B) {
		r := benchRunner(b, stubSender{})
		env := generatedIDEnv("bench-delivered")
		b.ReportAllocs()
		for b.Loop() {
			del := &stubDelivery{env: env}
			_ = r.HandleDelivery(context.Background(), del)
		}
	})

	b.Run("retried", func(b *testing.B) {
		r := benchRunner(b, stubSender{err: shared.ErrUnavailable})
		// A stable dedup key keeps the message COUNTABLE, so it retries through
		// the ledger-backed backoff instead of poisoning on the first failure.
		env := stableCountLessEnv("bench-retried")
		b.ReportAllocs()
		for b.Loop() {
			del := &stubDelivery{env: env}
			_ = r.HandleDelivery(context.Background(), del)
		}
	})

	b.Run("abandoned", func(b *testing.B) {
		// One runner and one sender for the whole run; only the per-delivery
		// context is fresh, so the measurement is the dispatch path and not the
		// route construction around it.
		sender := &benchCancellingSender{}
		r := benchRunner(b, sender)
		env := generatedIDEnv("bench-abandoned")
		b.ReportAllocs()
		for b.Loop() {
			ctx, cancel := context.WithCancel(context.Background())
			sender.cancel = cancel
			_ = r.HandleDelivery(ctx, &stubDelivery{env: env})
			cancel()
		}
	})
}
