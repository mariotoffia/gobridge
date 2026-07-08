package transporttest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// SeededReceiver is a ports.Receiver pre-loaded with a fixed number of
// deliveries for conformance testing, paired with the DeliveryProbes that
// observe each one's broker-side disposition.
//
// The receiver MUST emit each seeded delivery exactly once, in Probes order,
// then block until its ctx is cancelled and return ctx.Err() or nil. If the
// emit callback returns an error the receiver MUST NOT settle that delivery; it
// MAY stop emitting and return the error from Run (as the prevailing adapters
// do) or continue — the suite tolerates either.
type SeededReceiver struct {
	// Receiver is the ports.Receiver under test.
	Receiver ports.Receiver
	// Probes observe the seeded deliveries, index-aligned with emit order:
	// Probes[i] observes the i-th delivery the receiver emits.
	Probes []DeliveryProbe
}

// ReceiverFactory builds a SeededReceiver carrying n independent, unsettled
// deliveries. Every call returns fresh state; the suite calls it once per
// subtest with the n that subtest needs.
type ReceiverFactory func(t *testing.T, n int) SeededReceiver

// RunReceiverConformanceTests runs the ports.Receiver emit-callback contract
// conformance suite against receivers produced by factory. It pins the
// contract documented on ports.Receiver: serial (non-overlapping) emit, no
// receiver self-settlement on either emit outcome, tolerance of settlement
// after Run returns, no emit after Run returns, and Run returning on ctx
// cancellation. All subtests are race-detector safe.
func RunReceiverConformanceTests(t *testing.T, factory ReceiverFactory, caps Caps) {
	t.Helper()

	t.Run("EmitsEachSeededDeliveryOnce", func(t *testing.T) {
		receiverEmitsAll(t, factory)
	})
	t.Run("EmitIsSerial", func(t *testing.T) {
		receiverEmitSerial(t, factory)
	})
	t.Run("EmitErrorDoesNotSelfSettle", func(t *testing.T) {
		receiverEmitErrorNoSettle(t, factory)
	})
	t.Run("EmitNilDoesNotSelfSettle", func(t *testing.T) {
		receiverEmitNilNoSettle(t, factory, caps)
	})
	t.Run("SettlementAfterRunReturns", func(t *testing.T) {
		receiverSettleAfterRun(t, factory)
	})
	t.Run("NoEmitAfterRunReturns", func(t *testing.T) {
		receiverNoEmitAfterRun(t, factory)
	})
	t.Run("RunReturnsOnCancel", func(t *testing.T) {
		receiverRunReturnsOnCancel(t, factory)
	})
}

// emitRecorder is the conformance suite's emit callback. It records the
// deliveries it receives, detects concurrent (overlapping) invocations, and can
// return a caller-chosen error per call.
type emitRecorder struct {
	mu         sync.Mutex
	deliveries []ports.Delivery

	inFlight int32 // current concurrent emit invocations
	maxConc  int32 // high-water mark of concurrent invocations

	delay  time.Duration     // artificial hold to widen the overlap window
	errFor func(i int) error // error to return for the i-th emit, or nil

	wantN  int
	gotAll chan struct{} // closed once wantN emits have been observed
}

func (r *emitRecorder) emit(_ context.Context, d ports.Delivery) error {
	cur := atomic.AddInt32(&r.inFlight, 1)
	for {
		m := atomic.LoadInt32(&r.maxConc)
		if cur <= m || atomic.CompareAndSwapInt32(&r.maxConc, m, cur) {
			break
		}
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	atomic.AddInt32(&r.inFlight, -1)

	r.mu.Lock()
	idx := len(r.deliveries)
	r.deliveries = append(r.deliveries, d)
	n := len(r.deliveries)
	r.mu.Unlock()

	if r.gotAll != nil && n == r.wantN {
		close(r.gotAll)
	}
	if r.errFor != nil {
		return r.errFor(idx)
	}
	return nil
}

func (r *emitRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deliveries)
}

func (r *emitRecorder) captured() []ports.Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ports.Delivery, len(r.deliveries))
	copy(out, r.deliveries)
	return out
}

// runReceiver launches sr.Receiver.Run in a goroutine and returns a cancel
// function and a wait function. wait blocks until Run returns (failing the test
// if it does not within a bounded time) and yields Run's error.
func runReceiver(t *testing.T, r ports.Receiver, emit func(context.Context, ports.Delivery) error) (context.Context, func(), func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, emit) }()
	wait := func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return within 2s")
			return nil
		}
	}
	return ctx, cancel, wait
}

func waitForEmits(t *testing.T, rec *emitRecorder) {
	t.Helper()
	select {
	case <-rec.gotAll:
	case <-time.After(2 * time.Second):
		t.Fatalf("receiver emitted %d deliveries, want %d within 2s", rec.count(), rec.wantN)
	}
}

func receiverEmitsAll(t *testing.T, factory ReceiverFactory) {
	const n = 3
	sr := factory(t, n)
	rec := &emitRecorder{wantN: n, gotAll: make(chan struct{})}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)
	cancel()
	_ = wait()

	if got := rec.count(); got != n {
		t.Fatalf("emit called %d times, want %d", got, n)
	}
	if got := atomic.LoadInt32(&rec.maxConc); got > 1 {
		t.Fatalf("emit invoked concurrently (max %d in flight), want serial", got)
	}
}

func receiverEmitSerial(t *testing.T, factory ReceiverFactory) {
	const n = 5
	sr := factory(t, n)
	// A non-trivial delay widens the window in which a buggy concurrent emit
	// would overlap, so the -race build and the maxConc check can catch it.
	rec := &emitRecorder{wantN: n, gotAll: make(chan struct{}), delay: 2 * time.Millisecond}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)
	cancel()
	_ = wait()

	if got := atomic.LoadInt32(&rec.maxConc); got != 1 {
		t.Fatalf("max concurrent emit = %d, want exactly 1 (serial invocation)", got)
	}
}

func receiverEmitErrorNoSettle(t *testing.T, factory ReceiverFactory) {
	const n = 1
	sr := factory(t, n)
	emitErr := errors.New("pipeline rejected delivery")
	rec := &emitRecorder{
		wantN:  n,
		gotAll: make(chan struct{}),
		errFor: func(int) error { return emitErr },
	}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)

	// The core invariant: an emit that returns an error MUST leave the delivery
	// unsettled — the receiver must not ack/delete it.
	if got := sr.Probes[0].Disposition(); got != DispositionNone {
		t.Fatalf("after emit error, disposition = %s, want none (receiver self-settled)", got)
	}
	if got := sr.Probes[0].BrokerOps(); got != 0 {
		t.Fatalf("after emit error, broker ops = %d, want 0 (receiver self-settled)", got)
	}
	cancel()
	_ = wait() // Run may have already returned the emit error, or return on cancel.
}

func receiverEmitNilNoSettle(t *testing.T, factory ReceiverFactory, caps Caps) {
	const n = 1
	sr := factory(t, n)
	rec := &emitRecorder{wantN: n, gotAll: make(chan struct{})}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)

	// emit returned nil: ownership transferred to the pipeline. The receiver
	// MUST NOT settle the delivery itself.
	if got := sr.Probes[0].Disposition(); got != DispositionNone {
		t.Fatalf("after emit nil, disposition = %s, want none (receiver self-settled)", got)
	}

	// Settlement is performed exclusively through the Delivery handle. Prove the
	// handle the receiver emitted still settles.
	captured := rec.captured()
	if len(captured) != n {
		t.Fatalf("captured %d deliveries, want %d", len(captured), n)
	}
	if err := captured[0].Ack(context.Background()); err != nil {
		t.Fatalf("Ack via emitted handle: %v", err)
	}
	if got := sr.Probes[0].Disposition(); got != DispositionAcked {
		t.Fatalf("after pipeline Ack, disposition = %s, want acked", got)
	}
	_ = caps
	cancel()
	_ = wait()
}

func receiverSettleAfterRun(t *testing.T, factory ReceiverFactory) {
	const n = 1
	sr := factory(t, n)
	rec := &emitRecorder{wantN: n, gotAll: make(chan struct{})}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)
	captured := rec.captured()

	// Cancel and wait for Run to fully return BEFORE settling: a delivery must
	// remain safe to settle after the receive loop is gone.
	cancel()
	_ = wait()

	if len(captured) != n {
		t.Fatalf("captured %d deliveries, want %d", len(captured), n)
	}
	if err := captured[0].Ack(context.Background()); err != nil {
		t.Fatalf("Ack after Run returned: %v", err)
	}
	if got := sr.Probes[0].Disposition(); got != DispositionAcked {
		t.Fatalf("post-Run Ack disposition = %s, want acked", got)
	}
	if got := sr.Probes[0].BrokerOps(); got != 1 {
		t.Fatalf("post-Run Ack broker ops = %d, want 1", got)
	}
}

func receiverNoEmitAfterRun(t *testing.T, factory ReceiverFactory) {
	const n = 2
	sr := factory(t, n)
	rec := &emitRecorder{wantN: n, gotAll: make(chan struct{})}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	waitForEmits(t, rec)
	cancel()
	_ = wait()

	countAtReturn := rec.count()
	// Give any stray goroutine a chance to (wrongly) emit after Run returned.
	time.Sleep(50 * time.Millisecond)
	if got := rec.count(); got != countAtReturn {
		t.Fatalf("emit invoked after Run returned: count went %d -> %d", countAtReturn, got)
	}
}

func receiverRunReturnsOnCancel(t *testing.T, factory ReceiverFactory) {
	// Zero seeded deliveries: Run should immediately proceed to blocking on ctx
	// and return promptly once cancelled.
	sr := factory(t, 0)
	rec := &emitRecorder{}

	_, cancel, wait := runReceiver(t, sr.Receiver, rec.emit)
	cancel()
	err := wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want nil or context.Canceled", err)
	}
}
