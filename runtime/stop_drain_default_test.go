package runtime_test

import (
	"context"
	stdruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// liveCtxSender is a Sender whose Send blocks until release fires and then
// CAPTURES ctx.Err() at that exact moment. The captured value is the
// discriminator for the SIGTERM drain-before-cancel fix: the send happens on the
// runtime work context, so if Stop drains BEFORE cancelling (fixed bridge.go) the
// context is still live at release → ctx.Err()==nil; if Stop cancels FIRST
// (unfixed) the context is already Canceled at release → ctx.Err()==context.Canceled.
// It deliberately ignores ctx.Done() while blocked so the observed error reflects
// solely the drain-vs-cancel ORDERING, not a cooperative early return.
type liveCtxSender struct {
	entered chan struct{} // closed once when Send begins
	release chan struct{} // test closes to unblock Send
	errAt   chan error    // buffered(1): ctx.Err() captured at release
	once    sync.Once
}

func newLiveCtxSender() *liveCtxSender {
	return &liveCtxSender{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		errAt:   make(chan error, 1),
	}
}

func (s *liveCtxSender) Send(ctx context.Context, _ ports.OutboundMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	s.errAt <- ctx.Err()
	return nil
}

// workCtxReceiver mirrors how a real transport receiver dispatches: it emits each
// delivery on the runtime WORK context it was handed in Run (the context Stop
// cancels), NOT on the caller's emit context. This is the faithful shape for the
// drain-vs-cancel discriminator: only when deliveries ride the work context does
// Stop's cancel() actually reach an in-flight send, so capturing ctx.Err() in the
// sender distinguishes drain-before-cancel from cancel-first. (FakeReceiver.Emit
// dispatches on the caller's context, which Stop never cancels, so it cannot see
// the ordering — the reason this test defines its own receiver.)
type workCtxReceiver struct {
	ready chan struct{}
	mu    sync.Mutex
	emit  func(context.Context, ports.Delivery) error
	ctx   context.Context
}

func newWorkCtxReceiver() *workCtxReceiver {
	return &workCtxReceiver{ready: make(chan struct{})}
}

func (r *workCtxReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	r.ctx = ctx
	close(r.ready)
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// EmitWork dispatches del on the captured WORK context, exactly as a production
// receiver's Run loop does.
func (r *workCtxReceiver) EmitWork(del ports.Delivery) error {
	<-r.ready
	r.mu.Lock()
	emit := r.emit
	ctx := r.ctx
	r.mu.Unlock()
	return emit(ctx, del)
}

// TestStop_DrainsInFlightBeforeCancel_ByDefault proves the SIGTERM fix
// (runtime/bridge.go): Stop settles in-flight deliveries BEFORE cancelling the
// runtime context EVEN WITHOUT WithStopQuiesce.
//
// This test DISCRIMINATES drain-before-cancel from wait-after-cancel (finding
// #11): the earlier version passed on unfixed bridge.go because the fake sender
// ignored ctx and Stop blocked in wg.Wait() after cancel either way. Two things
// make this version fail on unfixed and pass on fixed:
//
//   - The delivery rides the runtime WORK context (workCtxReceiver), so cancel()
//     actually reaches the in-flight send. The sender captures ctx.Err() at the
//     moment of release: on fixed bridge.go the drain happens on a LIVE context
//     (cancel is downstream of the drain) → ctx.Err()==nil; on unfixed bridge.go
//     cancel fired first → ctx.Err()==context.Canceled → the assertion fatals.
//   - A fake-clock timer barrier gates the release so that on unfixed cancel has
//     provably already fired before we release (the first NEW timer after Stop
//     starts is downstream of cancel), and on fixed we release while WaitQuiescent
//     is draining on the live context.
//
// Fully deterministic: no real timers/sleeps drive the runtime — the fake clock
// is the only time source, advanced explicitly to close the MinQuiet window. The
// wall-clock spins below are pure test synchronisation (the fake clock is frozen
// until the explicit Advance), so the timer baseline is stable once captured.
func TestStop_DrainsInFlightBeforeCancel_ByDefault(t *testing.T) {
	fake := clocktest.New()
	sender := newLiveCtxSender()
	t.Cleanup(func() {
		select {
		case <-sender.release:
		default:
			close(sender.release)
		}
	})

	// NOTE: no WithStopQuiesce — this is the default lifecycle. Fake clock drives
	// all runtime timing so the drain window is closed deterministically.
	rt := goruntime.New(
		goruntime.WithInstanceID("sigterm-default"),
		goruntime.WithClock(fake),
	)
	cfg, _, _ := helperQuiescentRoute("r1", nil)
	recv := newWorkCtxReceiver()
	if err := rt.AddRoute(cfg, recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Emit a delivery ON THE WORK CONTEXT; Send blocks on release (InFlight == 1).
	// We deliberately do NOT use WaitRouteReady: its internal clk.After(1s) poll
	// leaks a fake timer only SOMETIMES (when the route is not ready on the first
	// loop check), which made an absolute timer count flaky. Emitting straight
	// through the receiver's captured work ctx needs no readiness poll.
	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"}))
	go func() { _ = recv.EmitWork(del) }()
	<-sender.entered

	// Baseline: the in-flight delivery arms its own fake timers (boundedSend
	// SendTimeout ceiling, chain budget). The fake clock is frozen, so once those
	// are all registered the count is fixed — capture it as the pre-Stop baseline.
	base := settledFakeTimerCount(fake)

	stopDone := make(chan error, 1)
	go func() {
		// Generous ctx so the deadline fallback does not fire; the fake clock only
		// advances by the amounts we drive below, well under this budget.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopDone <- rt.Stop(stopCtx)
	}()

	// DRAIN BARRIER: wait for the FIRST new fake timer registered after Stop began.
	//   - fixed:   Stop enters WaitQuiescent on a LIVE ctx and arms a MinQuiet
	//              quiet-window timer → base+1. Releasing now captures a live ctx.
	//   - unfixed: Stop cancels first; the first post-Stop timer is downstream of
	//              cancel (the supervisor reacting to the cancelled receiver), so
	//              reaching base+1 GUARANTEES cancel already fired — the release
	//              below then captures context.Canceled and the assertion fatals.
	waitFakeTimerAtLeast(t, fake, base+1,
		"Stop must register a drain (or post-cancel) timer after starting")

	// Stop must NOT have returned yet: the in-flight delivery is still blocked.
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a delivery was still in flight; default Stop did not drain before cancel")
	default:
	}

	// Release the delivery. On fixed bridge.go the runtime context is still live
	// (cancel runs only after the drain settles) → ctx.Err()==nil. On unfixed
	// bridge.go cancel already fired → ctx.Err()==context.Canceled → fatal.
	close(sender.release)
	if err := <-sender.errAt; err != nil {
		t.Fatalf("delivery drained on a CANCELLED context (ctx.Err()=%v): Stop cancelled before draining in-flight work", err)
	}

	// Close the MinQuiet quiet window so WaitQuiescent returns and Stop finishes.
	// Advance in small MinQuiet-sized steps until Stop returns; no real sleeps
	// drive the runtime — only the fake clock moves.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.Advance(60 * time.Millisecond)
		select {
		case err := <-stopDone:
			if err != nil {
				t.Fatalf("Stop after drain: %v", err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop did not complete after the in-flight delivery drained")
		}
	}
}

// settledFakeTimerCount returns the fake clock's active-timer count once it has
// held steady across a short window. Because the caller never advances the fake
// clock while measuring, the count only ever grows as the in-flight delivery
// registers its timers and then stays fixed — a stable reading means every
// delivery timer is registered, giving a baseline that excludes any timer Stop
// will later add.
func settledFakeTimerCount(fake *clocktest.Fake) int {
	last := fake.TimerCount()
	stableSince := time.Now()
	for time.Since(stableSince) < 50*time.Millisecond {
		stdruntime.Gosched()
		if c := fake.TimerCount(); c != last {
			last = c
			stableSince = time.Now()
		}
	}
	return last
}

// waitFakeTimerAtLeast spins (test-synchronisation only; the fake clock is not
// advanced here) until the fake clock has at least want active timers.
func waitFakeTimerAtLeast(t *testing.T, fake *clocktest.Fake, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fake.TimerCount() < want && time.Now().Before(deadline) {
		stdruntime.Gosched()
	}
	if got := fake.TimerCount(); got < want {
		t.Fatalf("%s: expected >= %d fake timer(s), got %d", what, want, got)
	}
}
