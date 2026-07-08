package transport_test

// Deterministic test for audit chunk C18, finding 3: the DETACHED ingress
// dispatch must be bounded by MaxDispatchDuration UNCONDITIONALLY — it
// must not depend on the request context carrying a deadline. A bare
// http.Server installs no request-context deadline, so before the fix the
// detached dispatch context never cancelled and a wedged downstream
// leaked one goroutine + in-memory delivery per stuck request. Here the
// request context has NO deadline (only cancellation, for cleanup) yet the
// dispatch context must still cancel within the configured cap.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestChunk18_Receiver_DetachedDispatchBoundedWithoutTimeoutHandler(t *testing.T) {
	// Tiny cap keeps the test fast and deterministic: context.WithTimeout
	// uses the real monotonic clock (it is not injectable), so a small
	// real duration — not a sleep — is the correct primitive here.
	const maxDispatch = 60 * time.Millisecond
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(),
		ports.ReceiverSpec{ID: "bound-bare", Config: transport.Config{MaxDispatchDuration: maxDispatch}}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// emit models the real pipeline hand-off: it captures the DETACHED
	// dispatch context and returns immediately. Nothing ever settles the
	// delivery, so the only thing that can cancel the dispatch context is
	// its own MaxDispatchDuration cap.
	gotCtx := make(chan context.Context, 1)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(dctx context.Context, _ ports.Delivery) error {
			gotCtx <- dctx
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	// Bare request context: cancellable (so the handler goroutine can be
	// reclaimed at cleanup) but carrying NO deadline — exactly what a
	// plain http.Server with only Read/WriteTimeout produces.
	clientCtx, cancelClient := context.WithCancel(context.Background())
	t.Cleanup(cancelClient)
	if _, ok := clientCtx.Deadline(); ok {
		t.Fatal("test bug: client context must have no deadline")
	}

	req := httptest.NewRequest("POST", "/transport/http/receivers/bound-bare/messages",
		strings.NewReader(`{"subject":"t.bound","payload":{}}`)).WithContext(clientCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		factory.Handler().ServeHTTP(rec, req)
		close(served)
	}()
	t.Cleanup(func() {
		cancelClient()
		wait.RequireClosed(t, served, 2*time.Second)
	})

	dctx := wait.RequireReceive(t, gotCtx, 2*time.Second)

	// The dispatch context is bounded even though the request context was
	// not: it must carry a deadline...
	if _, ok := dctx.Deadline(); !ok {
		t.Fatal("detached dispatch context has no deadline: it must be bounded by max_dispatch_duration")
	}
	// ...and that deadline must actually FIRE on its own (the cap), with
	// no TimeoutHandler and without the test cancelling anything.
	select {
	case <-dctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("detached dispatch context did not cancel within max_dispatch_duration: it is unbounded")
	}
	if err := dctx.Err(); err != context.DeadlineExceeded {
		t.Fatalf("dispatch context must cancel via deadline, got %v", err)
	}
}
