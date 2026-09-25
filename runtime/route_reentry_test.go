package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// reentrantReceiver is a source transport whose Run FAILS the first time and
// then blocks until the runtime context ends — the shape of a queue that was
// briefly unreachable. It deliberately does NOT implement Close(ctx), matching
// the SQS, MQTT and HTTP receivers, whose broker clients are owned by the
// session rather than the receiver.
type reentrantReceiver struct {
	mu     sync.Mutex
	runs   int
	runCh  chan int // buffered: signals each Run entry
	failed bool
}

func newReentrantReceiver() *reentrantReceiver {
	return &reentrantReceiver{runCh: make(chan int, 8)}
}

func (r *reentrantReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.runs++
	n := r.runs
	first := !r.failed
	r.failed = true
	r.mu.Unlock()
	r.runCh <- n
	if first {
		return errors.New("source unreachable")
	}
	<-ctx.Done()
	return ctx.Err()
}

// closableReentrantReceiver is the same source with a Close(ctx) — the shape of
// the Service Bus and AMQP receivers, which own a link RouteRunner.Run closes on
// exit. Close ends one Run, not the receiver (the ports.Receiver contract), so a
// supervised restart re-enters this same instance. closesAtEntry records how
// many Closes preceded the latest Run entry.
type closableReentrantReceiver struct {
	*reentrantReceiver
	closes        atomic.Int32
	closesAtEntry atomic.Int32
}

func (r *closableReentrantReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.closesAtEntry.Store(r.closes.Load())
	return r.reentrantReceiver.Run(ctx, emit)
}

func (r *closableReentrantReceiver) Close(context.Context) error { r.closes.Add(1); return nil }

// TestSuperviseRoute_FailedReceiverRestartsRouteInIsolation pins the per-route
// isolation contract for both receiver shapes: after a fault the route backs off
// and re-enters the SAME receiver, while the runtime stays healthy and the
// process keeps serving every other route. A receiver with Close(ctx) is closed
// when its run ends and re-attached by the next Run, so it gets the same
// restart as one the route runner never closes.
func TestSuperviseRoute_FailedReceiverRestartsRouteInIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withClose bool
	}{
		{name: "without_close"},
		{name: "with_close", withClose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := clocktest.New()
			rt := goruntime.New(
				goruntime.WithInstanceID("route-isolation-"+tc.name),
				goruntime.WithClock(fake),
			)
			cfg, _, sender := helperQuiescentRoute("r1", nil)
			base := newReentrantReceiver()
			var (
				recv     ports.Receiver = base
				closable *closableReentrantReceiver
			)
			if tc.withClose {
				closable = &closableReentrantReceiver{reentrantReceiver: base}
				recv = closable
			}
			require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
			require.NoError(t, rt.Start(context.Background()))
			t.Cleanup(func() { _ = rt.Stop(context.Background()) })

			require.Equal(t, 1, waitRunEntry(t, base.runCh), "the route must run once before failing")

			// The supervisor waits out a jittered backoff on the injected clock;
			// advance past its ceiling so the retry is due.
			require.Eventually(t, func() bool {
				fake.Advance(time.Second)
				select {
				case n := <-base.runCh:
					return n == 2
				default:
					return false
				}
			}, 5*time.Second, 5*time.Millisecond,
				"a failed receiver must be re-entered after a fault (per-route isolation)")

			if closable != nil {
				assert.Equal(t, int32(1), closable.closesAtEntry.Load(),
					"the failed run must close the receiver exactly once before the restart re-enters it")
			}
			assert.False(t, rt.Terminal(),
				"one route fault must not make the whole runtime terminal while the route is retryable")
			assert.True(t, rt.Healthy(),
				"per-route isolation must leave the global healthy flag untouched")
		})
	}
}

// waitRunEntry returns the sequence number of the next Run entry.
func waitRunEntry(t *testing.T, runs <-chan int) int {
	t.Helper()
	select {
	case n := <-runs:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("route runner never entered the receiver's Run")
		return 0
	}
}
