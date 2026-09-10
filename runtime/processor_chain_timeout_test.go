package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestRunChain_OuterSwallowsInnerTimeout_RefusesSuccessMerge proves finding 2:
// when an inner processor times out (its goroutine is abandoned by design and
// keeps running) and an OUTER best-effort processor swallows the resulting
// ErrProcessorTimeout — returning nil — RunChain must NOT report success. A
// success return would drive the runner into mergeProcessedEnvelope, which
// iterates the chain envelope's header map while the abandoned goroutine is
// still mutating it: a fatal concurrent map read/write. RunChain instead refuses
// the success path while any processor goroutine remains outstanding, returning
// ErrProcessorTimeout tagged reason=processor-abandoned so the delivery is
// retried and never merged.
//
// Fails without the fix (neutralise the outstanding>0 guard in RunChain):
// RunChain returns nil.
func TestRunChain_OuterSwallowsInnerTimeout_RefusesSuccessMerge(t *testing.T) {
	fake := clocktest.New()
	const timeout = 30 * time.Second

	innerStarted := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the abandoned goroutine exit at teardown

	inner := &FakeProcessor{
		NameVal: "inner-timeout",
		ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
			close(innerStarted)
			<-release // ignore cancellation; stay abandoned and alive (outstanding>0)
			return nil
		},
	}
	outer := &FakeProcessor{
		NameVal: "outer-swallow",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			_ = next(ctx, env) // best-effort middleware: swallow the inner timeout
			return nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "abandon-merge"})

	errCh := make(chan error, 1)
	go func() {
		errCh <- route.RunChain(context.Background(), []ports.Processor{outer, inner}, env,
			route.WithChainTimeout(timeout),
			route.WithChainClock(fake),
		)
	}()

	<-innerStarted // inner running: its budget is armed, outer's is disarmed under next()
	waitForFakeTimers(t, fake, 1)
	fake.Advance(timeout) // fire inner's per-processor budget

	var got error
	waitFor(t, 3*time.Second, "RunChain returns", func() bool {
		select {
		case got = <-errCh:
			return true
		default:
			return false
		}
	})

	if got == nil {
		t.Fatal("RunChain returned nil while an abandoned processor is still live; " +
			"the success-path merge must be refused to avoid a concurrent map read/write")
	}
	if !errors.Is(got, shared.ErrProcessorTimeout) {
		t.Fatalf("expected ErrProcessorTimeout, got %v", got)
	}
	be, ok := shared.AsBridgeError(got)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", got)
	}
	if reason := be.Context["reason"]; reason != "processor-abandoned" {
		t.Fatalf("expected reason=processor-abandoned, got %v", reason)
	}
}

// TestRunChain_AbandonedInnerDoesNotRaceLiveOuterFrame is the finding-2
// intra-chain (sibling-frame) regression guard. RunChain gives each processor
// frame a private clone of the envelope and merges a frame's mutations back onto
// the caller's envelope ONLY once that frame's goroutine has cleanly returned. So
// when an inner processor times out and is abandoned — ignoring cancellation and
// continuing to write headers — while its OUTER frame, after next() returns the
// timeout, ALSO keeps writing headers, the two goroutines mutate two distinct
// clones and can never trigger a fatal "concurrent map writes". The test drives a
// guaranteed overlap window of both live writers and asserts under -race.
//
// Fails without the fix (pass the caller's env to every frame instead of a
// per-frame clone): the race detector / runtime reports a concurrent map write,
// because the abandoned inner writer and the live outer writer share one header
// map. The outstanding>0 guard still makes RunChain return ErrProcessorTimeout.
func TestRunChain_AbandonedInnerDoesNotRaceLiveOuterFrame(t *testing.T) {
	fake := clocktest.New()
	const timeout = 30 * time.Second

	innerStarted := make(chan struct{})
	outerResumed := make(chan struct{})
	releaseInner := make(chan struct{})
	releaseOuter := make(chan struct{})
	var innerOnce, outerOnce sync.Once
	stopInner := func() { innerOnce.Do(func() { close(releaseInner) }) }
	stopOuter := func() { outerOnce.Do(func() { close(releaseOuter) }) }
	t.Cleanup(stopInner) // abandoned inner exits only at teardown
	t.Cleanup(stopOuter)

	var innerWrites, outerWrites atomic.Int64

	inner := &FakeProcessor{
		NameVal: "inner-abandoned-writer",
		ProcessFn: func(_ context.Context, env *messaging.Envelope, _ ports.ProcessorFunc) error {
			close(innerStarted)
			for {
				select {
				case <-releaseInner:
					return context.Canceled
				default:
					env.SetHeader("inner-key", 1) // keep mutating the header map
					innerWrites.Add(1)
				}
			}
		},
	}
	outer := &FakeProcessor{
		NameVal: "outer-live-writer",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			_ = next(ctx, env) // returns ErrProcessorTimeout once inner's budget fires
			close(outerResumed)
			for {
				select {
				case <-releaseOuter:
					return nil // best-effort swallow, but mutate after next() (contract violation)
				default:
					env.SetHeader("outer-key", 1) // live writer, concurrent with the abandoned inner
					outerWrites.Add(1)
				}
			}
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "intra-chain-race"})
	errCh := make(chan error, 1)
	go func() {
		errCh <- route.RunChain(context.Background(), []ports.Processor{outer, inner}, env,
			route.WithChainTimeout(timeout),
			route.WithChainClock(fake),
		)
	}()

	<-innerStarted // inner is looping; its per-processor budget is armed
	waitForFakeTimers(t, fake, 1)
	fake.Advance(timeout) // fire inner's budget -> outer's next() returns, outer starts writing
	<-outerResumed

	// Both frames are now writing their own clones. Wait until each has made
	// substantial progress so their executions provably overlap — giving the race
	// detector a real window — then release the outer frame.
	waitFor(t, 3*time.Second, "both frames writing concurrently", func() bool {
		return innerWrites.Load() >= 2000 && outerWrites.Load() >= 2000
	})
	stopOuter()

	var got error
	waitFor(t, 3*time.Second, "RunChain returns", func() bool {
		select {
		case got = <-errCh:
			return true
		default:
			return false
		}
	})
	if !errors.Is(got, shared.ErrProcessorTimeout) {
		t.Fatalf("expected ErrProcessorTimeout while inner remains abandoned, got %v", got)
	}
}

// TestRunChain_PerProcessorTimeout_SummedBeyondBudgetSucceeds proves finding 3
// (the per-processor budget is genuinely per-processor, not a shared shrinking
// deadline): three processors, each taking under the per-processor timeout but
// together exceeding it, all succeed. The budget measures each processor's OWN
// time — it is disarmed while a processor delegates to next() — so processor 0
// is not charged for the whole downstream chain.
//
// Fails without the fix (neutralise the budget disarm in wrappedNext): processor
// 0's budget keeps running across the chain, fires once the cumulative time
// exceeds the timeout, and RunChain returns ErrProcessorTimeout.
func TestRunChain_PerProcessorTimeout_SummedBeyondBudgetSucceeds(t *testing.T) {
	fake := clocktest.New()
	const (
		timeout = 30 * time.Second
		n       = 3
		own     = timeout * 2 / 5 // 12s each: < timeout, but 3×12s = 36s > timeout
	)

	starts := make([]chan struct{}, n)
	proceeds := make([]chan struct{}, n)
	procs := make([]ports.Processor, n)
	for i := 0; i < n; i++ {
		starts[i] = make(chan struct{})
		proceeds[i] = make(chan struct{})
		i := i
		procs[i] = &FakeProcessor{
			NameVal: fmt.Sprintf("p%d", i),
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				close(starts[i]) // budget i is armed by the time Process runs
				select {
				case <-proceeds[i]:
				case <-ctx.Done():
					return ctx.Err()
				}
				return next(ctx, env)
			},
		}
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "summed-budget"})

	errCh := make(chan error, 1)
	go func() {
		errCh <- route.RunChain(context.Background(), procs, env,
			route.WithChainTimeout(timeout),
			route.WithChainClock(fake),
		)
	}()

	// Drive each processor: wait until it is running (its budget armed), let its
	// OWN time elapse (< timeout), then let it delegate to the next stage.
	for i := 0; i < n; i++ {
		select {
		case <-starts[i]:
		case <-time.After(3 * time.Second):
			t.Fatalf("processor %d did not start", i)
		}
		waitForFakeTimers(t, fake, 1)
		fake.Advance(own)
		close(proceeds[i])
	}

	var got error
	waitFor(t, 3*time.Second, "RunChain returns", func() bool {
		select {
		case got = <-errCh:
			return true
		default:
			return false
		}
	})
	if got != nil {
		t.Fatalf("processors each under the per-processor timeout must all succeed even when "+
			"their combined time exceeds it, got %v", got)
	}
}

// TestRunChain_PerProcessorTimeout_RealOverrunEmitsMetric proves the second half
// of finding 3: a genuine per-processor overrun (root context NOT cancelled) is
// classified processor-timeout and emits MetricProcessorTimeouts — it is no
// longer misclassified as shutdown-grace (which would suppress the metric).
//
// Fails without the fix (neutralise the budget branch to always classify
// shutdown-grace and skip the metric): reason is not processor-timeout and the
// metric is never emitted.
func TestRunChain_PerProcessorTimeout_RealOverrunEmitsMetric(t *testing.T) {
	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	const timeout = 5 * time.Second

	started := make(chan struct{})
	proc := &FakeProcessor{
		NameVal: "slow",
		ProcessFn: func(ctx context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
			close(started)
			<-ctx.Done() // overruns; unwinds only once the budget cancels it
			return ctx.Err()
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "overrun"})

	errCh := make(chan error, 1)
	go func() {
		errCh <- route.RunChain(context.Background(), []ports.Processor{proc}, env,
			route.WithChainTimeout(timeout),
			route.WithChainMetrics(rec),
			route.WithChainRouteID("r1"),
			route.WithChainClock(fake),
		)
	}()

	<-started
	waitForFakeTimers(t, fake, 1)
	fake.Advance(timeout) // fire the per-processor budget with the root still live

	var got error
	waitFor(t, 3*time.Second, "RunChain returns", func() bool {
		select {
		case got = <-errCh:
			return true
		default:
			return false
		}
	})

	if !errors.Is(got, shared.ErrProcessorTimeout) {
		t.Fatalf("expected ErrProcessorTimeout, got %v", got)
	}
	be, ok := shared.AsBridgeError(got)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", got)
	}
	if reason := be.Context["reason"]; reason != "processor-timeout" {
		t.Fatalf("a genuine per-processor overrun must classify processor-timeout, got %v", reason)
	}
	if n := len(rec.FindEntries(shared.MetricProcessorTimeouts)); n != 1 {
		t.Fatalf("MetricProcessorTimeouts emitted %d times, want 1", n)
	}
}

// TestRouteRunner_OuterSwallowsInnerTimeout_MergeDoesNotRace is the finding-2
// integration guard exercising the ACTUAL concurrent map access through the full
// runner. An inner processor times out and is abandoned by design (its goroutine
// keeps mutating the chain envelope's header map forever). An OUTER best-effort
// processor swallows the resulting ErrProcessorTimeout and returns nil. Before
// the fix RunChain reported success, so the runner ran mergeProcessedEnvelope,
// which iterates the chain envelope's header map (HeadersSnapshot) concurrently
// with the abandoned writer: a fatal "concurrent map read and map write".
//
// With the fix RunChain refuses the success path while a processor goroutine is
// still outstanding (reason=processor-abandoned), so the runner takes the error
// path: the delivery is retried, the merge never runs and the sender is never
// called — no data race. Neutralising the outstanding>0 guard in RunChain makes
// mergeProcessedEnvelope race the abandoned writer and the race detector fails
// this test.
func TestRouteRunner_OuterSwallowsInnerTimeout_MergeDoesNotRace(t *testing.T) {
	const deliveries = 16

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	defer closeStop()

	var started sync.WaitGroup
	started.Add(deliveries)
	var startMu sync.Mutex
	signalled := 0

	inner := &FakeProcessor{
		NameVal: "inner-timeout",
		ProcessFn: func(_ context.Context, env *messaging.Envelope, _ ports.ProcessorFunc) error {
			startMu.Lock()
			if signalled < deliveries {
				signalled++
				started.Done()
			}
			startMu.Unlock()
			// Abandoned by design: ignore cancellation and keep writing ever-new
			// keys into the chain envelope's header map. If the merge is not
			// refused, the runner iterates this same map concurrently.
			for i := 0; ; i++ {
				select {
				case <-stop:
					return context.Canceled
				default:
					env.SetHeader("late-"+strconv.Itoa(i), i)
				}
			}
		},
	}
	outer := &FakeProcessor{
		NameVal: "outer-swallow",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			_ = next(ctx, env) // best-effort middleware: swallow the inner timeout
			return nil
		},
	}

	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		// Small real timeout so the inner processor is abandoned quickly.
		cfg.Policy.ProcessorTimeout = 30 * time.Millisecond
		cfg.Processors = []ports.Processor{outer, inner}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	dels := make([]*FakeDelivery, deliveries)
	for i := range dels {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "msg-merge-race-" + strconv.Itoa(i),
			Payload: []byte("data"),
		})
		dels[i] = NewFakeDelivery(env)
		if err := receiver.Emit(ctx, dels[i]); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// Wait until every inner processor goroutine is live and mutating so the
	// race window with a would-be merge is actually open.
	waitDone := make(chan struct{})
	go func() { started.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("inner processors did not all start")
	}

	// The success-path merge must be refused: every delivery ends on the error
	// path (retried) and the sender is never called.
	waitFor(t, 5*time.Second, "all deliveries retried on the abandoned-merge path", func() bool {
		for _, d := range dels {
			if !d.IsRetried() {
				return false
			}
		}
		return true
	})
	if n := sender.SentCount(); n != 0 {
		t.Fatalf("sender called %d times; an abandoned-processor chain must not merge and dispatch", n)
	}

	// Let the abandoned writers unwind; passing under -race is the assertion.
	closeStop()
}
