package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestRunChain_ShutdownGrace_BoundsCtxIgnoringProcessor verifies finding
// #13: when the parent context is cancelled (route shutting down) a
// processor that ignores ctx.Done() does NOT hang invokeProcessor. The
// chain waits at most the configured shutdown grace (driven here by a
// fake clock), then releases the in-flight slot and returns a
// shutdown-grace timeout — distinct from a steady-state processor
// timeout via the "shutdown-grace-exceeded" reason.
func TestRunChain_ShutdownGrace_BoundsCtxIgnoringProcessor(t *testing.T) {
	fake := clocktest.New()

	// release lets the deliberately ctx-ignoring processor finally exit
	// at test teardown so the abandoned goroutine is not leaked.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	proc := &FakeProcessor{
		NameVal: "ignores-cancel",
		ProcessFn: func(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
			<-release // never observes ctx.Done()
			return nil
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-shutdown"})

	// Parent already cancelled: the route is shutting down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	const grace = 2 * time.Second
	errCh := make(chan error, 1)
	go func() {
		errCh <- route.RunChain(ctx, []ports.Processor{proc}, env,
			route.WithChainTimeout(30*time.Second),
			route.WithChainShutdownGrace(grace),
			route.WithChainClock(fake),
		)
	}()

	// The grace timer is registered once invokeProcessor enters the
	// parent-cancelled branch; advance the fake clock past it.
	waitForFakeTimers(t, fake, 1)
	fake.Advance(grace)

	var got error
	waitFor(t, 3*time.Second, "RunChain returns within the shutdown grace", func() bool {
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
	if be.Class != shared.ErrorTransient {
		t.Fatalf("expected Transient class, got %q", be.Class)
	}
	if reason := be.Context["reason"]; reason != "shutdown-grace-exceeded" {
		t.Fatalf("expected reason shutdown-grace-exceeded, got %v", reason)
	}
}

// TestRunChain_ShutdownGrace_ReturnsProcessorErrorWhenItUnwinds verifies
// the fast path of the bounded-grace branch: a processor that DOES
// observe cancellation before the grace elapses returns its own error
// (here ctx.Err()), not the shutdown-grace timeout.
func TestRunChain_ShutdownGrace_ReturnsProcessorErrorWhenItUnwinds(t *testing.T) {
	fake := clocktest.New()
	sentinel := errors.New("unwound on cancel")

	proc := &FakeProcessor{
		NameVal: "honours-cancel",
		ProcessFn: func(ctx context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
			<-ctx.Done() // observes cancellation promptly
			return sentinel
		},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-unwind"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var got error
	done := make(chan struct{})
	go func() {
		got = route.RunChain(ctx, []ports.Processor{proc}, env,
			route.WithChainTimeout(30*time.Second),
			route.WithChainShutdownGrace(2*time.Second),
			route.WithChainClock(fake),
		)
		close(done)
	}()

	waitFor(t, 3*time.Second, "RunChain returns the processor's own error", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})

	if !errors.Is(got, sentinel) {
		t.Fatalf("expected processor sentinel error, got %v", got)
	}
	if errors.Is(got, shared.ErrProcessorTimeout) {
		t.Fatalf("processor that honoured cancellation must not be misclassified as a timeout: %v", got)
	}
}
