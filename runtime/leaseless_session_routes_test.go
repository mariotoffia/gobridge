package runtime_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
)

// leaselessBindingSession is how the builder registers a session that a binding
// names: exclusive, with its connect deferred until it holds a lease.
func leaselessBindingSession(sessionID string) runsession.Config {
	return runsession.Config{SessionID: sessionID, Exclusive: true, ConnectAfterLease: true}
}

// A runtime with no lease store writes a direct_hold route's permanent send
// failure to the DLQ under the session its binding names. Nothing can grant
// that session a lease, so fencing the write on one refused it forever.
func TestLeaselessRuntime_DirectHoldDeadLettersUnderTheBindingSession(t *testing.T) {
	store := NewFakeDLQStore()
	rec := &ports.RecordingExporter{}
	rt := goruntime.New(goruntime.WithInstanceID("leaseless-binding-dlq"),
		goruntime.WithDLQStore(store), goruntime.WithMetrics(rec))
	sender := NewFakeSender()
	sender.SendErr = shared.NewBridgeError("PERM", shared.ErrorPermanent, "permanent failure")
	sess := NewFakeSession()
	cfg := goruntime.RouteConfig{
		ID: "binding-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDLQ,
			OnExpired:          routing.ExpiredDrop,
		},
		Bindings:           []routing.DestinationBinding{{ID: "b1", Address: "out/addr", SessionID: "dst-session"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	recv := NewFakeReceiver()
	// The builder adds a route with no session block on its binding's session,
	// then registers that session as the binding's sender.
	if err := rt.AddRoute(cfg, recv, sender, sess, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.RegisterSessionSender(leaselessBindingSession("dst-session"), sess, sender); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	startRuntime(t, rt)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "perm-1", Subject: "leaseless"})
	del := NewFakeDelivery(env)
	if err := recv.Emit(context.Background(), del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, 2*time.Second, "the delivery is settled", func() bool { return del.IsAcked() || del.IsRetried() })

	if got := store.Count(); got != 1 {
		t.Fatalf("DLQ entries = %d, want 1 (DLQWriteFailures=%d)", got, len(rec.FindEntries(shared.MetricDLQWriteFailures)))
	}
	store.mu.Lock()
	entry := store.Entries[0]
	store.mu.Unlock()
	if entry.SessionID() != "dst-session" || entry.RouteID() != "binding-route" {
		t.Fatalf("DLQ entry session/route = %q/%q, want dst-session/binding-route", entry.SessionID(), entry.RouteID())
	}
	if !del.IsAcked() {
		t.Fatal("a dead-lettered delivery must be acknowledged")
	}
	if len(rec.FindEntries(shared.MetricDLQEntries)) == 0 {
		t.Fatal("DLQEntries was not counted for the written record")
	}
	if n := len(rec.FindEntries(shared.MetricDLQWriteFailures)); n != 0 {
		t.Fatalf("DLQWriteFailures counted %d times, want 0", n)
	}
}

// A runtime with no lease store dead-letters and acknowledges a delivery the
// broker still hands a binding's session for a subscription it removed. The
// session acknowledges such a delivery only when the write returns nil, so a
// refused write kept it, and retried it, forever.
func TestLeaselessRuntime_RemovedSubscriptionOnBindingSessionIsDeadLettered(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("leaseless-removed-sub"), goruntime.WithDLQStore(store))
	sess := newDeadLetterSession()
	if err := rt.RegisterSessionSender(leaselessBindingSession("dst-session"), sess, NewFakeSender()); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	startRuntime(t, rt)

	entry := writeHeldDelivery(t, sess, store, "stale/#")
	if entry.ErrorCode() != string(shared.ErrCodeSubscriptionRemoved) || entry.SessionID() != "dst-session" {
		t.Fatalf("DLQ entry code/session = %q/%q, want %s/dst-session",
			entry.ErrorCode(), entry.SessionID(), shared.ErrCodeSubscriptionRemoved)
	}
}

// A shared_outbox route whose binding names a session sender gets an outbox
// drainer that waits for that session's lease. A runtime with no lease store
// never grants one, so the drainer would skip every cycle while the source is
// acknowledged after each persist: the runtime refuses the route instead.
func TestLeaselessRuntime_SharedOutboxBindingSessionIsRefused(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("leaseless-outbox"), goruntime.WithOutboxStore(NewFakeOutboxStore()))
	sess, sender := NewFakeSession(), NewFakeSender()
	cfg := goruntime.RouteConfig{
		ID: "outbox-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		Bindings:           []routing.DestinationBinding{{ID: "b1", Address: "out/addr", SessionID: "dst-session"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
	if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.RegisterSessionSender(leaselessBindingSession("dst-session"), sess, sender); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		_ = rt.Stop(context.Background())
		t.Fatal("Start accepted a shared_outbox route whose binding session can never drain")
	}
	var ve *goruntime.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Start error = %v, want a *ValidationError", err)
	}
	want := []string{`route "outbox-route": shared_outbox invalid: no LeaseStore configured for binding ` +
		`session "dst-session"; its outbox drainer waits for a lease that nothing grants, so persisted ` +
		`records never drain (a LeaseStore is required)`}
	if got := ve.Errors(); !slices.Equal(got, want) {
		t.Fatalf("validation errors = %q, want %q", got, want)
	}
}
