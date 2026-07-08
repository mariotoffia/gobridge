package ports_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// TestDeliveryScope_ReleaseRunsCallbacksLIFO proves OnRelease callbacks all fire
// on Release, in LIFO order (mirroring defer), so a processor's cleanup nests
// correctly against other registrations.
func TestDeliveryScope_ReleaseRunsCallbacksLIFO(t *testing.T) {
	_, scope := ports.WithDeliveryScope(context.Background())

	var order []int
	scope.OnRelease(func() { order = append(order, 1) })
	scope.OnRelease(func() { order = append(order, 2) })
	scope.OnRelease(func() { order = append(order, 3) })

	scope.Release()

	if len(order) != 3 {
		t.Fatalf("expected 3 callbacks fired, got %d (%v)", len(order), order)
	}
	// LIFO: last registered runs first.
	if order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("expected LIFO order [3 2 1], got %v", order)
	}
}

// TestDeliveryScope_ReleaseIdempotent proves a second Release is a no-op: each
// registered callback runs exactly once even if Release is called twice (the
// runtime defers Release; a processor could also call it).
func TestDeliveryScope_ReleaseIdempotent(t *testing.T) {
	_, scope := ports.WithDeliveryScope(context.Background())

	var calls atomic.Int64
	scope.OnRelease(func() { calls.Add(1) })

	scope.Release()
	scope.Release()

	if got := calls.Load(); got != 1 {
		t.Fatalf("callback fired %d times, want exactly 1 (Release must be idempotent)", got)
	}
}

// TestDeliveryScope_OnReleaseAfterReleaseRunsInline is the abandonment
// close-guard: a processor goroutine abandoned on a chain timeout may register
// AFTER the runtime already released the scope. The callback must still run
// (inline, on the caller) so a resource is never stranded.
func TestDeliveryScope_OnReleaseAfterReleaseRunsInline(t *testing.T) {
	_, scope := ports.WithDeliveryScope(context.Background())
	scope.Release()

	ran := false
	scope.OnRelease(func() { ran = true })

	if !ran {
		t.Fatal("OnRelease after Release must run the callback inline so no release is stranded")
	}
}

// TestDeliveryScope_NilCallbackIgnored proves OnRelease(nil) is a no-op and does
// not panic on Release.
func TestDeliveryScope_NilCallbackIgnored(t *testing.T) {
	_, scope := ports.WithDeliveryScope(context.Background())
	scope.OnRelease(nil)
	scope.Release() // must not panic
}

// TestDeliveryScope_From proves the ctx round-trip: WithDeliveryScope installs a
// scope that DeliveryScopeFrom recovers, and a bare context carries none.
func TestDeliveryScope_From(t *testing.T) {
	ctx, scope := ports.WithDeliveryScope(context.Background())
	got, ok := ports.DeliveryScopeFrom(ctx)
	if !ok || got != scope {
		t.Fatalf("DeliveryScopeFrom did not recover the installed scope (ok=%v)", ok)
	}
	if _, ok := ports.DeliveryScopeFrom(context.Background()); ok {
		t.Fatal("a bare context must carry no delivery scope")
	}
}

// TestDeliveryScope_ConcurrentRegisterAndRelease exercises the mutex under -race:
// many goroutines register callbacks while Release runs concurrently. Every
// callback must fire exactly once (either queued-then-run by Release, or run
// inline when it lands after Release) with no data race and no lost callback.
func TestDeliveryScope_ConcurrentRegisterAndRelease(t *testing.T) {
	const n = 256
	_, scope := ports.WithDeliveryScope(context.Background())

	var fired atomic.Int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			scope.OnRelease(func() { fired.Add(1) })
		}()
	}
	// Release concurrently with the registrations. The mutex partitions every
	// callback into exactly one drain path: callbacks appended before Release
	// marks the scope done run from Release's snapshot; callbacks arriving after
	// run inline on the registrant goroutine.
	relDone := make(chan struct{})
	go func() {
		defer close(relDone)
		scope.Release()
	}()

	// wg.Wait covers every inline-run callback (it fires before OnRelease
	// returns); <-relDone covers every snapshot callback (the Release goroutine
	// runs them asynchronously). Reading fired without awaiting the Release
	// goroutine would race its drain, so wait for both before asserting.
	wg.Wait()
	<-relDone

	if got := fired.Load(); got != n {
		t.Fatalf("fired %d callbacks, want %d (no callback may be lost under concurrent register/release)", got, n)
	}
}
