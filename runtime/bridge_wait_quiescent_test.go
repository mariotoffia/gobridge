package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// closeOnce returns a function that closes ch at most once, safe to
// call from both the test body and t.Cleanup.
func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// helperQuiescentRoute builds a route with a FakeSender whose Send
// blocks on the returned release channel, so tests can hold a
// delivery in flight on demand.
func helperQuiescentRoute(id string, release <-chan struct{}) (goruntime.RouteConfig, *FakeReceiver, *FakeSender) {
	cfg := goruntime.RouteConfig{
		ID: id,
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  4,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	sender := NewFakeSender()
	if release != nil {
		sender.SendFn = func(env *domain.Envelope) error {
			<-release
			return nil
		}
	}
	return cfg, NewFakeReceiver(), sender
}

// TestWaitRouteReady_ReturnsOnStartedClose verifies that WaitRouteReady
// returns promptly once the runner's Started() channel closes, rather
// than waiting for a 50 ms poll boundary.
func TestWaitRouteReady_ReturnsOnStartedClose(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("wrr-t1"))
	cfg, recv, sender := helperQuiescentRoute("r1", nil)
	if err := rt.AddRoute(cfg, recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := rt.WaitRouteReady(ctx, "r1"); err != nil {
		t.Fatalf("WaitRouteReady: %v", err)
	}
	elapsed := time.Since(start)
	// Should return well before the 1s sanity fallback fires.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("WaitRouteReady took %v; expected event-driven return (<500ms)", elapsed)
	}
}

// TestWaitQuiescent_ReturnsOnIdleEvent verifies that a watched route's
// IdleChanged event unblocks WaitQuiescent rather than the previous
// 10 ms DeepHealth poll.
func TestWaitQuiescent_ReturnsOnIdleEvent(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := closeOnce(release)
	rt := goruntime.New(goruntime.WithInstanceID("wq-t1"))
	cfg, recv, sender := helperQuiescentRoute("r1", release)
	if err := rt.AddRoute(cfg, recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		releaseOnce() // unblock any still-running deliveries
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := rt.WaitRouteReady(readyCtx, "r1"); err != nil {
		t.Fatalf("WaitRouteReady: %v", err)
	}

	// Emit a delivery; it will block in Send until release fires.
	del := NewFakeDelivery(&domain.Envelope{ID: "m1", Subject: "t"})
	if err := recv.Emit(context.Background(), del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Start the waiter; it must observe InFlight>0 and block.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()

	done := make(chan error, 1)
	go func() {
		done <- rt.WaitQuiescent(waitCtx, goruntime.QuiescenceOptions{
			Routes:   []string{"r1"},
			MinQuiet: 30 * time.Millisecond,
		})
	}()

	// Release the in-flight delivery → InFlight → 0 fires IdleChanged.
	releaseOnce()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitQuiescent: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("WaitQuiescent did not return after idle event")
	}
}

// TestWaitQuiescent_WatchedRoutesFilter verifies that the watched
// subset argument causes the waiter to ignore unrelated routes.
func TestWaitQuiescent_WatchedRoutesFilter(t *testing.T) {
	releaseA := make(chan struct{}) // never closed → route a stays busy
	releaseAOnce := closeOnce(releaseA)
	rt := goruntime.New(goruntime.WithInstanceID("wq-t2"))

	cfgA, recvA, senderA := helperQuiescentRoute("route-a", releaseA)
	if err := rt.AddRoute(cfgA, recvA, senderA, nil, nil); err != nil {
		t.Fatalf("AddRoute a: %v", err)
	}
	cfgB, recvB, senderB := helperQuiescentRoute("route-b", nil)
	if err := rt.AddRoute(cfgB, recvB, senderB, nil, nil); err != nil {
		t.Fatalf("AddRoute b: %v", err)
	}

	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		releaseAOnce()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := rt.WaitRouteReady(readyCtx, "route-a"); err != nil {
		t.Fatalf("WaitRouteReady a: %v", err)
	}
	if err := rt.WaitRouteReady(readyCtx, "route-b"); err != nil {
		t.Fatalf("WaitRouteReady b: %v", err)
	}

	// Make route-a permanently busy.
	if err := recvA.Emit(context.Background(),
		NewFakeDelivery(&domain.Envelope{ID: "m-a", Subject: "t"})); err != nil {
		t.Fatalf("Emit a: %v", err)
	}

	// WaitQuiescent watching only route-b must return promptly.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	start := time.Now()
	if err := rt.WaitQuiescent(waitCtx, goruntime.QuiescenceOptions{
		Routes:   []string{"route-b"},
		MinQuiet: 30 * time.Millisecond,
	}); err != nil {
		t.Fatalf("WaitQuiescent: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("WaitQuiescent on idle route took %v; expected <500ms", elapsed)
	}
}

// TestWaitQuiescent_ContextCancel verifies that a cancelled context
// aborts the wait with ctx.Err() rather than hanging.
func TestWaitQuiescent_ContextCancel(t *testing.T) {
	release := make(chan struct{}) // never closed
	releaseOnce := closeOnce(release)
	rt := goruntime.New(goruntime.WithInstanceID("wq-t3"))
	cfg, recv, sender := helperQuiescentRoute("r1", release)
	if err := rt.AddRoute(cfg, recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		releaseOnce()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := rt.WaitRouteReady(readyCtx, "r1"); err != nil {
		t.Fatalf("WaitRouteReady: %v", err)
	}

	if err := recv.Emit(context.Background(),
		NewFakeDelivery(&domain.Envelope{ID: "m1", Subject: "t"})); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	waitCtx, waitCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rt.WaitQuiescent(waitCtx, goruntime.QuiescenceOptions{
			MinQuiet: 50 * time.Millisecond,
		})
	}()
	waitCancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected context error, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("WaitQuiescent did not return after context cancel")
	}
}
