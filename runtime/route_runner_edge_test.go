package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type emptyResolver struct{}

func (emptyResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]routing.DispatchPlan, error) {
	return []routing.DispatchPlan{}, nil
}

func TestDirectHold_EmptyPlans_DoesNotPanic(t *testing.T) {
	sender := NewFakeSender()
	receiver := NewFakeReceiver()
	rec := &ports.RecordingExporter{}

	runner := goruntime.NewRouteRunnerFromConfig(goruntime.RouteRunnerConfig{
		RouteID:  "empty-plans-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: receiver,
		Sender:   sender,
		Resolver: emptyResolver{},
		Metrics:  rec,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = runner.Run(ctx)
	}()

	<-receiver.Ready()

	env := &messaging.Envelope{ID: "test-empty-plans", Payload: []byte("data")}
	del := NewFakeDelivery(env)

	err := receiver.Emit(ctx, del)
	if err != nil {
		t.Fatalf("unexpected emit error: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery retried or acked", func() bool {
		return del.IsRetried() || del.IsAcked()
	})

	panics := rec.FindEntries(shared.MetricDeliveryPanics)
	if len(panics) > 0 {
		t.Fatal("empty plans should be handled as an error, not trigger a panic recovery path")
	}
}
