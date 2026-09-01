package runtime_test

import (
	"context"
	"errors"
	"sync"
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
// the Service Bus and AMQP receivers, which own a link that RouteRunner.Run
// closes on exit. Closing makes the instance single-use, so a supervised restart
// has nothing left to re-enter.
type closableReentrantReceiver struct {
	*reentrantReceiver
}

func (r *closableReentrantReceiver) Close(context.Context) error { return nil }

// TestSuperviseRoute_ReceiverWithoutClose_RestartsRouteInIsolation pins the
// per-route isolation contract for the receivers that actually get it: a source
// the route runner never closes is re-entered after a fault, so one bad route
// backs off and retries while the runtime stays healthy and the process keeps
// serving every other route.
func TestSuperviseRoute_ReceiverWithoutClose_RestartsRouteInIsolation(t *testing.T) {
	fake := clocktest.New()
	rt := goruntime.New(
		goruntime.WithInstanceID("route-isolation-reentry"),
		goruntime.WithClock(fake),
	)
	cfg, _, sender := helperQuiescentRoute("r1", nil)
	recv := newReentrantReceiver()
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	require.Equal(t, 1, waitRunEntry(t, recv.runCh), "the route must run once before failing")

	// The supervisor waits out a jittered backoff on the injected clock; advance
	// past its ceiling so the retry is due.
	require.Eventually(t, func() bool {
		fake.Advance(time.Second)
		select {
		case n := <-recv.runCh:
			return n == 2
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond,
		"a receiver the route runner never closes must be re-entered after a fault (per-route isolation)")

	assert.False(t, rt.Terminal(),
		"one route fault must not make the whole runtime terminal while the route is retryable")
	assert.True(t, rt.Healthy(),
		"per-route isolation must leave the global healthy flag untouched")
}

// TestSuperviseRoute_ClosableReceiver_EscalatesToProcessRestart pins the OTHER
// half of the contract, the one an operator must plan for: when the route runner
// owns a Close(ctx)-capable source it closes it on exit, so the instance is
// single-use. There is no factory to rebuild it from, so the route is terminal
// and the runtime escalates — the documented backstop is a process restart with
// freshly-built transports, not a per-route retry.
func TestSuperviseRoute_ClosableReceiver_EscalatesToProcessRestart(t *testing.T) {
	fake := clocktest.New()
	rt := goruntime.New(
		goruntime.WithInstanceID("route-isolation-single-use"),
		goruntime.WithClock(fake),
	)
	cfg, _, sender := helperQuiescentRoute("r1", nil)
	recv := &closableReentrantReceiver{reentrantReceiver: newReentrantReceiver()}
	require.NoError(t, rt.AddRoute(cfg, recv, sender, nil, nil))
	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	require.Equal(t, 1, waitRunEntry(t, recv.runCh), "the route must run once before failing")

	require.Eventually(t, func() bool {
		fake.Advance(time.Second)
		return rt.Terminal()
	}, 5*time.Second, 5*time.Millisecond,
		"a closed single-use receiver cannot be re-entered, so the route must escalate to a terminal runtime")

	assert.Equal(t, 1, recv.runEntries(),
		"a closed single-use receiver must never be re-run; the backstop is a process restart")
}

func (r *reentrantReceiver) runEntries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
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
