package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies RunChain succeeds when the processor list is nil.
func TestRunChain_Empty(t *testing.T) {
	env := &domain.Envelope{ID: "msg-1"}
	if err := runtime.RunChain(context.Background(), nil, env); err != nil {
		t.Fatalf("empty chain should succeed, got %v", err)
	}
}

// Verifies a single processor in the chain is invoked once.
func TestRunChain_Single(t *testing.T) {
	p := &FakeProcessor{NameVal: "p1"}
	env := &domain.Envelope{ID: "msg-1"}

	if err := runtime.RunChain(context.Background(), []ports.Processor{p}, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.CalledCount() != 1 {
		t.Fatalf("expected processor called 1 time, got %d", p.CalledCount())
	}
}

// Verifies processors run in slice order when each calls next.
func TestRunChain_Order(t *testing.T) {
	var order []string
	makeProcessor := func(name string) ports.Processor {
		return &FakeProcessor{
			NameVal: name,
			ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
				order = append(order, name)
				return next(ctx, env)
			},
		}
	}

	processors := []ports.Processor{
		makeProcessor("first"),
		makeProcessor("second"),
		makeProcessor("third"),
	}

	env := &domain.Envelope{ID: "msg-1"}
	if err := runtime.RunChain(context.Background(), processors, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("wrong execution order: %v", order)
	}
}

// Verifies envelope mutations performed in a processor are visible to later steps and after the chain completes.
func TestRunChain_Mutation(t *testing.T) {
	p := &FakeProcessor{
		NameVal: "mutator",
		ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
			env.Subject = "mutated"
			return next(ctx, env)
		},
	}

	env := &domain.Envelope{ID: "msg-1", Subject: "original"}
	if err := runtime.RunChain(context.Background(), []ports.Processor{p}, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Subject != "mutated" {
		t.Fatalf("expected subject 'mutated', got %q", env.Subject)
	}
}

// Verifies RunChain returns the first processor error.
func TestRunChain_Error(t *testing.T) {
	p := &FakeProcessor{
		NameVal:    "failing",
		ProcessErr: fmt.Errorf("boom"),
	}

	env := &domain.Envelope{ID: "msg-1"}
	err := runtime.RunChain(context.Background(), []ports.Processor{p}, env)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected error 'boom', got %v", err)
	}
}

// Verifies a failing processor prevents subsequent processors from running.
func TestRunChain_ShortCircuit(t *testing.T) {
	p1 := &FakeProcessor{
		NameVal:    "blocker",
		ProcessErr: fmt.Errorf("blocked"),
	}
	p2 := &FakeProcessor{NameVal: "never"}

	env := &domain.Envelope{ID: "msg-1"}
	err := runtime.RunChain(context.Background(), []ports.Processor{p1, p2}, env)
	if err == nil {
		t.Fatal("expected error from short-circuit")
	}
	if p2.CalledCount() != 0 {
		t.Fatal("second processor should not have been called")
	}
}

// Verifies a panicking processor is recovered, returns ErrProcessorPanic
// (Permanent), and subsequent processors are not invoked.
func TestRunChain_PanicRecovered(t *testing.T) {
	panicker := &FakeProcessor{
		NameVal: "panicker",
		ProcessFn: func(_ context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
			panic("boom")
		},
	}
	follower := &FakeProcessor{NameVal: "follower"}
	terminalCalled := int32(0)
	terminal := &FakeProcessor{
		NameVal: "terminal",
		ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
			atomic.AddInt32(&terminalCalled, 1)
			return next(ctx, env)
		},
	}

	env := &domain.Envelope{ID: "msg-panic"}
	err := runtime.RunChain(context.Background(),
		[]ports.Processor{panicker, follower, terminal}, env,
		runtime.WithChainTimeout(time.Second),
	)
	if err == nil {
		t.Fatal("expected ErrProcessorPanic, got nil")
	}
	if !errors.Is(err, shared.ErrProcessorPanic) {
		t.Fatalf("expected ErrProcessorPanic, got %v", err)
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if be.Class != shared.ErrorPermanent {
		t.Fatalf("expected Permanent class, got %q", be.Class)
	}
	if follower.CalledCount() != 0 {
		t.Fatalf("follower must not run after panic, got %d calls", follower.CalledCount())
	}
	if atomic.LoadInt32(&terminalCalled) != 0 {
		t.Fatal("terminal must not run after panic")
	}
}

// Verifies a hanging processor is bounded by the per-processor timeout
// and returns ErrProcessorTimeout (Transient).
func TestRunChain_HangingProcessorTimesOut(t *testing.T) {
	hang := &FakeProcessor{
		NameVal: "hang",
		ProcessFn: func(ctx context.Context, _ *domain.Envelope, _ ports.ProcessorFunc) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	env := &domain.Envelope{ID: "msg-hang"}

	start := time.Now()
	err := runtime.RunChain(context.Background(), []ports.Processor{hang}, env,
		runtime.WithChainTimeout(50*time.Millisecond),
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ErrProcessorTimeout, got nil")
	}
	if !errors.Is(err, shared.ErrProcessorTimeout) {
		t.Fatalf("expected ErrProcessorTimeout, got %v", err)
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if be.Class != shared.ErrorTransient {
		t.Fatalf("expected Transient class, got %q", be.Class)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout should fire promptly, took %s", elapsed)
	}
}

// Verifies normal processor errors are returned verbatim and not
// misclassified as panic or timeout.
func TestRunChain_NormalErrorNotMisclassified(t *testing.T) {
	sentinel := errors.New("normal failure")
	p := &FakeProcessor{NameVal: "fail", ProcessErr: sentinel}

	env := &domain.Envelope{ID: "msg-1"}
	err := runtime.RunChain(context.Background(), []ports.Processor{p}, env)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if errors.Is(err, shared.ErrProcessorPanic) {
		t.Fatal("normal error must not be classified as panic")
	}
	if errors.Is(err, shared.ErrProcessorTimeout) {
		t.Fatal("normal error must not be classified as timeout")
	}
}

// Verifies the default timeout (30s) does not fire for fast processors.
func TestRunChain_HappyPathUnderDefaultTimeout(t *testing.T) {
	p := &FakeProcessor{
		NameVal: "fast",
		ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
			return next(ctx, env)
		},
	}
	env := &domain.Envelope{ID: "msg-1"}
	// No options: defaults should apply (30s timeout, no logger).
	if err := runtime.RunChain(context.Background(), []ports.Processor{p}, env); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}
