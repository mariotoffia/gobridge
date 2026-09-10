package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// cancelThenPanicResolver models what a shutdown does to a dependency the
// delivery pipeline is still using: the delivery context is torn down and the
// next call into the half-closed dependency panics.
type cancelThenPanicResolver struct{ cancel context.CancelFunc }

func (r *cancelThenPanicResolver) Resolve(context.Context, *messaging.Envelope) ([]routing.DispatchPlan, error) {
	r.cancel()
	panic("resolver touched a dependency the shutdown already tore down")
}

// TestDeliveryPanic_UnderCancellation_LeavesDeliveryUnsettled pins the last
// terminal path a bridge-initiated cancellation could reach: the panic
// recovery. A panic outside the processor chain (resolver, hook, tracer,
// metrics) is normally routed through the same replay-cap gate as every other
// failure, and for an adapter-generated identity — the common MQTT publish —
// that gate reports "already at the cap" on the FIRST occurrence, so the
// message is poisoned or, with no DLQ store, dropped.
//
// A panic that happens because the bridge is tearing itself down is not
// evidence that the message is bad. The delivery is left UNSETTLED so the
// source redelivers it after the restart.
//
// Mutation check: route a panic on a cancelled delivery context back through
// the poison gate and this fails — the delivery is acked and the message is
// gone.
func TestDeliveryPanic_UnderCancellation_LeavesDeliveryUnsettled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	receiver := NewFakeReceiver()
	rec := &ports.RecordingExporter{}
	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID: "panic-under-cancel",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 3,
			SendTimeout:       time.Second,
		},
		Receiver: receiver,
		Sender:   NewFakeSender(),
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: &cancelThenPanicResolver{cancel: cancel},
		// No DLQ store: every terminal decision on this route DISCARDS the
		// message, so an ack here is an irrecoverable loss.
		DLQ:     dlq.New(nil),
		Metrics: rec,
	})

	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "panic-cancel-1"})
	env.SetHeader(messaging.HeaderGeneratedID, "true") // uncountable: the loss-prone shape
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	// Run returns only after every in-flight delivery goroutine has finished,
	// so the settlement decision has already been made when this fires.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the resolver cancelled the delivery context")
	}

	if del.IsAcked() {
		t.Fatal("a delivery that panicked while the bridge was cancelling it was ACKed; " +
			"with no DLQ store the message is gone")
	}
	for _, e := range rec.FindEntries("MessagesDropped") {
		t.Fatalf("a cancelled delivery was counted as dropped: %v", e.Tags)
	}
}
