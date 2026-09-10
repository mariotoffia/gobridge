package tenant

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// liveInFlightTracker is a reader-capable tracker whose Usage returns the LIVE
// in-flight count (the running sum of IncrementInFlight deltas), unlike the
// settable-usage readerTracker in quota_test.go. It lets a test observe the
// tenant's accounting window directly.
type liveInFlightTracker struct {
	inFlight atomic.Int64
	messages atomic.Int64
}

var (
	_ ports.TenantUsageTracker = (*liveInFlightTracker)(nil)
	_ ports.TenantUsageReader  = (*liveInFlightTracker)(nil)
)

func (t *liveInFlightTracker) IncrementMessages(_ context.Context, _ string, count int64) error {
	t.messages.Add(count)
	return nil
}

func (t *liveInFlightTracker) IncrementInFlight(_ context.Context, _ string, delta int64) error {
	t.inFlight.Add(delta)
	return nil
}

func (t *liveInFlightTracker) Usage(_ context.Context, _ string) (ports.TenantUsage, error) {
	return ports.TenantUsage{InFlight: t.inFlight.Load()}, nil
}

func (t *liveInFlightTracker) current() int64 { return t.inFlight.Load() }

// TestProcess_InFlightAccountingSpansDelivery_ViaScope is the fix proof. It
// reproduces the runtime's delivery lifecycle: a fresh ports.DeliveryScope is
// installed per delivery and released only AFTER the (simulated) send — mirroring
// runtime doHandleDelivery, where the tenant is the last processor and the send
// happens in the RouteRunner after the chain returns.
//
// Because the tenant now registers its in-flight decrement on the scope (not a
// defer inside Process), the +1 stays live across the send window: N concurrent
// in-flight deliveries hold InFlight == N, and an (N+1)th delivery is rejected
// with ErrTenantQuotaExceeded against that live count.
//
// Fails without the fix (restore `defer release()` inside Process): the -1 fires
// the instant Process returns (mid-chain, before the send), so InFlight collapses
// to ~0 between deliveries, the InFlight==N assertion times out, and the (N+1)th
// delivery is admitted instead of throttled — exactly the no-op quota the finding
// describes.
func TestProcess_InFlightAccountingSpansDelivery_ViaScope(t *testing.T) {
	const ceiling = int64(3)
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxInFlight: ceiling}}
	tracker := &liveInFlightTracker{}
	p := mustNew(t, Config{}, WithValidator(v), WithUsageTracker(tracker))

	terminalNext := func(_ context.Context, _ *messaging.Envelope) error { return nil }

	sendGate := make(chan struct{}) // closed to let every in-flight "send" complete
	ready := make(chan struct{}, ceiling)
	var wg sync.WaitGroup

	// Launch `ceiling` concurrent deliveries. Each installs its own delivery
	// scope, runs the tenant chain (registers the -1 on the scope), signals
	// ready, then blocks in the simulated send BEFORE releasing the scope.
	for i := int64(0); i < ceiling; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, scope := ports.WithDeliveryScope(context.Background())
			defer scope.Release() // runtime defers this at doHandleDelivery return (post-send)

			if err := p.Process(ctx, envelope("acme", 0), terminalNext); err != nil {
				t.Errorf("in-flight delivery unexpectedly rejected: %v", err)
				ready <- struct{}{}
				return
			}
			ready <- struct{}{}
			<-sendGate // hold the delivery open across the "send"
		}()
	}

	// Wait until all `ceiling` deliveries have passed through the chain (each +1
	// has landed and its -1 is registered on the still-open scope).
	for i := int64(0); i < ceiling; i++ {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatalf("delivery %d did not reach the send window", i)
		}
	}

	// The accounting must span the send: all `ceiling` deliveries are in flight,
	// so InFlight is exactly the ceiling (no decrement has fired — no scope has
	// been released yet).
	require.Equal(t, ceiling, tracker.current(),
		"InFlight must reflect the concurrent in-flight sends, not collapse when Process returns")

	// An over-ceiling delivery reads the LIVE in-flight count and is rejected.
	overCtx, overScope := ports.WithDeliveryScope(context.Background())
	defer overScope.Release()
	nextCalled := false
	err := p.Process(overCtx, envelope("acme", 0), func(_ context.Context, _ *messaging.Envelope) error {
		nextCalled = true
		return nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrTenantQuotaExceeded,
		"a delivery over MaxInFlight must be rejected while sends are in flight")
	assert.False(t, nextCalled, "rejected delivery must not proceed down the chain")
	assert.Equal(t, ceiling, tracker.current(), "a rejected delivery must not change the in-flight count")

	// Release every in-flight send; the scope releases now fire the paired -1.
	close(sendGate)
	wg.Wait()
	assert.Eventually(t, func() bool { return tracker.current() == 0 }, time.Second, 5*time.Millisecond,
		"once every delivery settles, the in-flight count must drain back to zero")
}
