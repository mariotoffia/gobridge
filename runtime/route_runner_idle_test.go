package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// newIdleTestRunner builds a minimal RouteRunner wired to a FakeReceiver
// and a FakeSender whose SendFn blocks until the returned release
// channel fires. Callers use it to hold a delivery in-flight so they
// can observe InFlight transitions deterministically.
func newIdleTestRunner(t *testing.T) (*FakeReceiver, *route.RouteRunner, *FakeSender, chan struct{}) {
	t.Helper()
	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	release := make(chan struct{})
	sender.SendFn = func(env *messaging.Envelope) error {
		<-release
		return nil
	}
	cfg := route.RouteRunnerConfig{
		RouteID:  "idle-test",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(NewFakeDLQStore()),
	}
	runner := route.NewRouteRunnerFromConfig(cfg)
	return receiver, runner, sender, release
}

// TestRouteRunner_IdleChanged_FiresOnZeroTransition asserts that the
// channel returned by IdleChanged() is closed when the only in-flight
// delivery completes (InFlight transitions to zero).
func TestRouteRunner_IdleChanged_FiresOnZeroTransition(t *testing.T) {
	receiver, runner, _, release := newIdleTestRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Capture the idle channel BEFORE emitting (lost-wakeup safety).
	idle := runner.IdleChanged()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"})
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "inflight>0", func() bool { return runner.InFlight() == 1 })

	// Release the blocked Send — delivery completes, InFlight → 0.
	close(release)

	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatalf("IdleChanged did not close after InFlight → 0")
	}
	if runner.InFlight() != 0 {
		t.Fatalf("expected InFlight=0, got %d", runner.InFlight())
	}
}

// TestRouteRunner_IdleChanged_SwapsOnFire asserts that a fresh unclosed
// channel is installed after a transition fires.
func TestRouteRunner_IdleChanged_SwapsOnFire(t *testing.T) {
	receiver, runner, _, release := newIdleTestRunner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	first := runner.IdleChanged()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"})
	if err := receiver.Emit(ctx, NewFakeDelivery(env)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, 2*time.Second, "inflight>0", func() bool { return runner.InFlight() == 1 })
	close(release)

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatalf("first IdleChanged did not fire")
	}

	// After the fire, IdleChanged must return a NEW, not-yet-closed channel.
	second := runner.IdleChanged()
	if first == second {
		t.Fatalf("expected idleCh swap, same channel returned")
	}
	select {
	case <-second:
		t.Fatalf("second IdleChanged should not be closed yet")
	default:
	}
}

// TestRouteRunner_IdleChanged_NoFireWhenNotAtZero asserts that the idle
// channel does NOT close when an active delivery completes but another
// is still in flight (InFlight drops from 2 → 1, not to 0).
//
// Why ConcurrentSender and not FakeSender: FakeSender.Send serialises
// through its own mutex held for the duration of SendFn. With two
// concurrent deliveries, the Go scheduler picks one of them arbitrarily
// to hold the mutex, parking the other one inside mu.Lock (not on its
// release channel). close(r1) would then have no effect on the loser
// and the test would hang at count>1. ConcurrentSender has no such
// mutex, so each delivery blocks on its own release channel and close
// signals arrive where the test expects them.
func TestRouteRunner_IdleChanged_NoFireWhenNotAtZero(t *testing.T) {
	receiver := NewFakeReceiver()

	// Per-delivery release gates.
	r1 := make(chan struct{})
	r2 := make(chan struct{})
	sender := NewConcurrentSender(func(env *messaging.Envelope) error {
		switch env.ID {
		case "m1":
			<-r1
		case "m2":
			<-r2
		}
		return nil
	})
	cfg := route.RouteRunnerConfig{
		RouteID:  "idle-test-multi",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 4}.WithDefaults(),
		Receiver: receiver,
		Sender:   sender,
		DLQ:      dlq.New(NewFakeDLQStore()),
	}
	runner := route.NewRouteRunnerFromConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	idle := runner.IdleChanged()

	if err := receiver.Emit(ctx, NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"}))); err != nil {
		t.Fatalf("Emit m1: %v", err)
	}
	if err := receiver.Emit(ctx, NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m2", Subject: "t"}))); err != nil {
		t.Fatalf("Emit m2: %v", err)
	}
	waitFor(t, 2*time.Second, "inflight=2", func() bool { return runner.InFlight() == 2 })

	// Release only m1 — InFlight drops 2 → 1, MUST NOT fire idle.
	close(r1)
	waitFor(t, 2*time.Second, "inflight=1", func() bool { return runner.InFlight() == 1 })

	select {
	case <-idle:
		t.Fatalf("IdleChanged fired on 2→1 transition; must fire only on →0")
	case <-time.After(50 * time.Millisecond):
	}

	// Release m2 — InFlight → 0, idle MUST fire.
	close(r2)
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatalf("IdleChanged did not fire after InFlight → 0")
	}
}
