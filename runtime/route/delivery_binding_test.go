package route

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// --- test doubles (package route, kept distinct from dispatch_followups_test.go) ---

// overrideDelivery is a trusted internal delivery carrying an out-of-band
// binding-scoped route override — exactly the shape Runtime.InjectToBinding /
// InjectRedrive construct (syntheticDelivery). It satisfies the runner's
// unexported bindingOverrider contract, so only a white-box (package route)
// test can build one.
type overrideDelivery struct {
	stubDelivery
	binding string
}

func (d *overrideDelivery) BindingOverride() string { return d.binding }

// ackCtxDelivery records the ctx.Err() observed at Ack time and HONOURS
// cancellation (a cancelled ctx aborts the Ack) — modelling a real transport
// whose Ack fails once its delivery ctx is cancelled.
type ackCtxDelivery struct {
	env       *messaging.Envelope
	acked     bool
	ackCtxErr error
}

func (d *ackCtxDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *ackCtxDelivery) Ack(ctx context.Context) error {
	d.ackCtxErr = ctx.Err()
	if d.ackCtxErr != nil {
		return d.ackCtxErr
	}
	d.acked = true
	return nil
}
func (d *ackCtxDelivery) Retry(context.Context, time.Duration, error) error { return nil }
func (d *ackCtxDelivery) Extend(context.Context, time.Time) error           { return nil }

// gateSender blocks inside Send until release is closed, signalling entered on
// the first call so a test can observe the pipeline mid-flight.
type gateSender struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *gateSender) Send(context.Context, ports.OutboundMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

// TestHandleDelivery_OutOfBandBinding_UnknownBindingRejected proves finding 1:
// an OUT-OF-BAND binding override (operator DLQ redrive via
// Runtime.InjectToBinding, carried on a bindingOverrider delivery) whose
// recorded binding no longer exists on a reconfigured route is rejected as a
// PERMANENT shared.ErrNotFound BEFORE entering the pipeline — it never falls
// through to normal (fan-out) resolution. So the replay reaches NO binding
// (MessagesSent==0) and the delivery is never accepted (MessagesReceived==0),
// letting the admin DLQ redrive path restore/keep the DLQ entry.
//
// Fails without the fix: doHandleDelivery would strip headers, re-stamp the
// override, then directHold's unknown-binding fall-through resolves the single
// live binding-a and sends there (MessagesReceived==1, MessagesSent==1) — the
// silent fan-out the finding describes.
func TestHandleDelivery_OutOfBandBinding_UnknownBindingRejected(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:   stubSender{},
		Metrics:  rec,
		Bindings: []routing.DestinationBinding{{ID: "binding-a", Address: "devices/a"}},
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "redrive-1", Payload: []byte("p")})
	del := &overrideDelivery{stubDelivery: stubDelivery{env: env}, binding: "ghost-binding"}

	err := r.HandleDelivery(context.Background(), del)

	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected shared.ErrNotFound for an unknown out-of-band binding, got %v", err)
	}
	if got := countCounter(rec, shared.MetricMessagesReceived); got != 0 {
		t.Fatalf("MessagesReceived emitted %d; a rejected redrive must not enter the pipeline", got)
	}
	if got := countCounter(rec, shared.MetricMessagesSent); got != 0 {
		t.Fatalf("MessagesSent emitted %d; a rejected redrive must fan out to NO binding", got)
	}
	if del.acked {
		t.Fatal("rejected redrive must not ack (settle) the delivery")
	}
}

// TestHandleDelivery_OutOfBandBinding_KnownBindingPasses is the control: the
// SAME out-of-band override, when its binding still exists, is NOT rejected —
// the confinement guard is exact, never over-rejecting a live binding.
func TestHandleDelivery_OutOfBandBinding_KnownBindingPasses(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:   stubSender{},
		Metrics:  rec,
		Bindings: []routing.DestinationBinding{{ID: "binding-a", Address: "devices/a"}},
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "redrive-ok", Payload: []byte("p")})
	del := &overrideDelivery{stubDelivery: stubDelivery{env: env}, binding: "binding-a"}

	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("redrive to a live binding must succeed, got %v", err)
	}
	if got := countCounter(rec, shared.MetricMessagesSent); got != 1 {
		t.Fatalf("MessagesSent = %d, want 1 (confined send to binding-a)", got)
	}
}

// TestSendDirectHold_CancelledDeliveryCtx_StillAcks proves finding 4: after a
// successful send, the happy-path Ack must land even when the delivery ctx has
// already been cancelled (shutdown). settleContext strips the cancellation
// (keeping values) and applies a short bound, so a successful send is never
// left un-acked — which would force an avoidable transport redelivery.
//
// Fails without the fix: ackDelivery would issue del.Ack on the cancelled ctx,
// the Ack aborts (context.Canceled), sendDirectHold returns that error and the
// delivery is left un-acked.
func TestSendDirectHold_CancelledDeliveryCtx_StillAcks(t *testing.T) {
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "r1",
		Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:  stubSender{}, // send succeeds (ignores ctx: models a send already completed)
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ack-1", Payload: []byte("p")})
	del := &ackCtxDelivery{env: env}

	// The delivery ctx is already cancelled: shutdown cancelled it right after
	// the send succeeded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.sendDirectHold(ctx, del, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"})
	if err != nil {
		t.Fatalf("a successful send must still ack in the shutdown window, got %v", err)
	}
	if !del.acked {
		t.Fatal("expected the happy-path Ack to land despite the cancelled delivery ctx")
	}
	if del.ackCtxErr != nil {
		t.Fatalf("Ack observed a cancelled ctx (%v); settleContext must strip cancellation", del.ackCtxErr)
	}
}

// TestHandleDelivery_CountsInFlightForSynchronousInject proves finding 5: a
// synchronous admin inject (Runtime.Inject/InjectToBinding → HandleDelivery)
// participates in the SAME in-flight accounting the receive loop uses, so
// Runtime.InFlight()/WaitQuiescent can observe an in-progress inject and a
// reconfig cannot declare the route quiescent mid-redrive.
//
// Fails without the fix: HandleDelivery would not touch r.inFlight, so
// InFlight() stays 0 while a synchronous inject is blocked mid-send.
func TestHandleDelivery_CountsInFlightForSynchronousInject(t *testing.T) {
	sender := &gateSender{entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:   sender,
		Bindings: []routing.DestinationBinding{{ID: "b1", Address: "addr"}},
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "inflight-1", Payload: []byte("p")})

	if got := r.InFlight(); got != 0 {
		t.Fatalf("precondition: InFlight()=%d, want 0", got)
	}

	done := make(chan error, 1)
	go func() { done <- r.HandleDelivery(context.Background(), &stubDelivery{env: env}) }()

	// Block until the inject is mid-send; the delivery is now in flight.
	<-sender.entered
	if got := r.InFlight(); got != 1 {
		t.Fatalf("InFlight()=%d during a synchronous inject, want 1 "+
			"(WaitQuiescent would otherwise declare quiescence mid-redrive)", got)
	}

	close(sender.release)
	if err := <-done; err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	// The deferred decrement runs before HandleDelivery returns, so the count is
	// already back to zero once done fires.
	if got := r.InFlight(); got != 0 {
		t.Fatalf("InFlight()=%d after the inject settled, want 0", got)
	}
}
