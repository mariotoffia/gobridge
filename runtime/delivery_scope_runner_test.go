package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestRouteRunner_DeliveryScope_ReleaseSpansSend proves F3.2: the runtime
// installs a ports.DeliveryScope on the delivery context and releases it only
// once the WHOLE delivery finishes — AFTER the egress send. A processor that
// registers a release on the scope therefore has its callback fire after the
// send, NOT when Process returns mid-chain. This is the generic seam the tenant
// in-flight decrement rides on (F3.3): the accounting spans the send instead of
// collapsing to nanoseconds when the last processor returns.
//
// Fails without the fix (remove WithDeliveryScope/defer scope.Release() from
// doHandleDelivery): DeliveryScopeFrom returns ok=false, the release is never
// registered, and the post-send wait for `released` times out.
func TestRouteRunner_DeliveryScope_ReleaseSpansSend(t *testing.T) {
	var (
		released      atomic.Bool
		scopeObserved atomic.Bool
	)

	probe := &FakeProcessor{
		NameVal: "scope-probe",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			if scope, ok := ports.DeliveryScopeFrom(ctx); ok {
				scopeObserved.Store(true)
				scope.OnRelease(func() { released.Store(true) })
			}
			return next(ctx, env)
		},
	}

	receiver, sender, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
		cfg.Processors = []ports.Processor{probe}
	})

	sendEntered := make(chan struct{})
	unblock := make(chan struct{})
	var enteredOnce atomic.Bool
	sender.SendFn = func(_ *messaging.Envelope) error {
		if enteredOnce.CompareAndSwap(false, true) {
			close(sendEntered)
		}
		<-unblock // hold the send open so we can inspect the scope mid-delivery
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "scope-span"}))
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The send is now in progress (chain returned, Process delegated, directHold
	// called Send). The scope MUST NOT have been released yet — the delivery is
	// still in flight.
	select {
	case <-sendEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("send never started")
	}
	if !scopeObserved.Load() {
		t.Fatal("processor did not observe a delivery scope on ctx (runtime must install one)")
	}
	if released.Load() {
		t.Fatal("scope released while the send is still in flight; the release must span the send, not fire at Process return")
	}

	// Let the send complete; the scope release must fire once the delivery ends.
	close(unblock)
	waitFor(t, 2*time.Second, "delivery acked", del.IsAcked)
	waitFor(t, 2*time.Second, "scope released after send", released.Load)
}
