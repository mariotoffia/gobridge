package route

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSharedOutbox_NilOutboxStore_WedgesTerminally proves a shared_outbox route
// wired without an OutboxStore fails TERMINALLY instead of retrying forever.
//
// A missing store is a wiring defect, not a message fault: no redelivery can
// ever fix it, and the branch that handled it bypassed the replay cap
// (retryOrFallback never consults it), so a library caller that composed the
// route directly — the startup validator blocks this shape, so only the direct
// route.NewRouteRunner path reaches it — got an unbounded one-second retry loop
// per message behind green liveness. Wedging stops the route and escalates to
// the supervisor while leaving the delivery UNSETTLED, so nothing is acked or
// dropped.
func TestSharedOutbox_NilOutboxStore_WedgesTerminally(t *testing.T) {
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "no-outbox",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			MaxReplayAttempts: 2,
		},
		OutboxStore: nil, // the wiring defect under test
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		Metrics:     &ports.RecordingExporter{},
	})

	del := &stubDelivery{env: countLessEnv("no-outbox-1")}
	err := r.HandleDelivery(context.Background(), del)

	if !errors.Is(err, ErrRouteTerminal) {
		t.Fatalf("HandleDelivery = %v, want an error wrapping ErrRouteTerminal: a shared_outbox "+
			"route with no OutboxStore can never deliver, so retrying it forever hides the defect", err)
	}
	if !r.isWedged() {
		t.Fatal("the route must wedge so the supervisor escalates instead of the route flapping at its backoff cap")
	}
	if del.acked {
		t.Fatal("an undeliverable route must never ack the source")
	}
	if del.retried {
		t.Fatal("a wiring defect must not be retried: no redelivery can supply the missing store")
	}
}

// TestSharedOutbox_NilOutboxStore_RefusesFurtherDeliveries proves the wedge
// LATCHES: once the missing store is detected the route refuses subsequent
// deliveries with the same terminal error rather than re-entering the pipeline.
func TestSharedOutbox_NilOutboxStore_RefusesFurtherDeliveries(t *testing.T) {
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "no-outbox",
		Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "s1", Address: "addr"},
		},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		Metrics:  &ports.RecordingExporter{},
	})

	for i := range 3 {
		del := &stubDelivery{env: countLessEnv("no-outbox-loop")}
		if err := r.HandleDelivery(context.Background(), del); !errors.Is(err, ErrRouteTerminal) {
			t.Fatalf("delivery %d: HandleDelivery = %v, want ErrRouteTerminal", i, err)
		}
		if del.acked || del.retried {
			t.Fatalf("delivery %d settled (acked=%v retried=%v); a wedged route settles nothing",
				i, del.acked, del.retried)
		}
	}
}
