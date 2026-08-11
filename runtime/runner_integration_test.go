package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// TestIngressOnAttempt_ReflectsSourceReceiveCount proves finding 6: the ingress
// OnAttempt hook's Attempt reflects the SOURCE transport's redelivery count
// (e.g. SQS ApproximateReceiveCount) rather than a hardcoded 1, so a transport
// redelivery is reported as attempt 2+, not a fresh attempt 1. It falls back to
// 1 when the source carries no known redelivery-count header.
//
// Fails without the fix: the ingress OnAttempt Attempt is always 1, so the
// count=3 case reports 1.
func TestIngressOnAttempt_ReflectsSourceReceiveCount(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]any
		want    int
	}{
		{"sqs redelivery count 3", map[string]any{"sqs.ApproximateReceiveCount": 3}, 3},
		{"no count header -> first delivery", nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := &recordingHook{}
			_, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
				cfg.Hook = hook
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.MaxReplayAttempts = 10 // keep rc=3 below the poison cap
			})

			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "ingress-attempt",
				Payload: []byte("p"),
				Headers: tt.headers,
			})
			if err := runner.HandleDelivery(context.Background(), NewFakeDelivery(env)); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}

			var ingress *ports.DeliveryAttempt
			for _, a := range hook.Attempts() {
				a := a
				if a.Direction == ports.DirectionIngress {
					ingress = &a
					break
				}
			}
			if ingress == nil {
				t.Fatal("no ingress OnAttempt recorded")
			}
			if ingress.Attempt != tt.want {
				t.Fatalf("ingress OnAttempt.Attempt = %d, want %d", ingress.Attempt, tt.want)
			}
		})
	}
}

// TestWaitQuiescent_ObservesSynchronousInject proves finding 5: a synchronous
// admin inject (Runtime.InjectToBinding → runner.HandleDelivery) participates in
// the SAME in-flight accounting WaitQuiescent reads, so WaitQuiescent cannot
// declare a route quiescent while an inject is still in flight.
//
// Fails without the fix: HandleDelivery does not touch the runner's in-flight
// counter, so WaitQuiescent observes InFlight()==0 and returns nil mid-inject.
func TestWaitQuiescent_ObservesSynchronousInject(t *testing.T) {
	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	releaseOnce := closeOnce(release)

	rt := goruntime.New(goruntime.WithInstanceID("wq-inject"))
	sender := NewFakeSender()
	sender.SendFn = func(*messaging.Envelope) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}
	cfg := goruntime.RouteConfig{
		ID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxInFlight:        4,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
		Bindings:           []routing.DestinationBinding{{ID: "b1", Address: "addr"}},
	}
	recv := NewFakeReceiver()
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

	// Synchronous inject; it blocks inside Send until release fires.
	injectDone := make(chan error, 1)
	go func() {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "inj-1", Subject: "s", Payload: []byte("p")})
		injectDone <- rt.InjectToBinding(context.Background(), "r1", "b1", env)
	}()

	<-entered // the inject is now in flight (blocked mid-send)

	quiesced := make(chan error, 1)
	go func() {
		quiesced <- rt.WaitQuiescent(context.Background(), goruntime.QuiescenceOptions{
			Routes:   []string{"r1"},
			MinQuiet: 30 * time.Millisecond,
		})
	}()

	// While the inject is in flight WaitQuiescent must stay blocked.
	select {
	case err := <-quiesced:
		t.Fatalf("WaitQuiescent returned (%v) while a synchronous inject was in flight; "+
			"a reconfig could declare quiescence mid-redrive", err)
	case <-time.After(150 * time.Millisecond):
		// Correct: InFlight()>0 keeps the waiter blocked.
	}

	// Release the inject → InFlight → 0 → WaitQuiescent returns.
	releaseOnce()
	if err := <-injectDone; err != nil {
		t.Fatalf("InjectToBinding: %v", err)
	}
	select {
	case err := <-quiesced:
		if err != nil {
			t.Fatalf("WaitQuiescent after inject settled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitQuiescent did not return after the inject settled")
	}
}
