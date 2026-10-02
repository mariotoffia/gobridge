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

// outboxRoute is a shared_outbox route with the given bindings and session block.
func outboxRoute(id string, bindings ...routing.DestinationBinding) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID: id,
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		Bindings:           bindings,
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
}

// addOutboxBindingRoute adds a shared_outbox route whose only binding names
// "dst-session", and registers that session as a session sender with sessCfg
// the way the builder registers a binding's session.
func addOutboxBindingRoute(t *testing.T, rt *goruntime.Runtime, sessCfg runsession.Config, sess ports.Session, sender ports.Sender) {
	t.Helper()
	cfg := outboxRoute("outbox-route", routing.DestinationBinding{ID: "b1", Address: "out/addr", SessionID: "dst-session"})
	if err := rt.AddRoute(cfg, NewFakeReceiver(), sender, sess, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.RegisterSessionSender(sessCfg, sess, sender); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
}

// startForValidation starts rt and returns Start's error, stopping it at the
// end of the test when it started.
func startForValidation(t *testing.T, rt *goruntime.Runtime) error {
	t.Helper()
	err := rt.Start(context.Background())
	if err == nil {
		t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	}
	return err
}

// requireValidationErrors fails unless err is a *ValidationError carrying
// exactly want.
func requireValidationErrors(t *testing.T, err error, want []string) {
	t.Helper()
	var ve *goruntime.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Start error = %v, want a *ValidationError", err)
	}
	if got := ve.Errors(); !slices.Equal(got, want) {
		t.Fatalf("validation errors = %q, want %q", got, want)
	}
}

// A shared_outbox route whose binding names a session sender gets an outbox
// drainer that waits for that session's lease. A runtime with no lease store
// never grants one, so the drainer would skip every cycle while the source is
// acknowledged after each persist: the runtime refuses the route instead.
func TestLeaselessRuntime_SharedOutboxBindingSessionIsRefused(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("leaseless-outbox"), goruntime.WithOutboxStore(NewFakeOutboxStore()))
	addOutboxBindingRoute(t, rt, leaselessBindingSession("dst-session"), NewFakeSession(), NewFakeSender())
	requireValidationErrors(t, startForValidation(t, rt), []string{`route "outbox-route": shared_outbox invalid: ` +
		`no LeaseStore configured for binding session "dst-session"; its outbox drainer waits for a lease ` +
		`that nothing grants, so persisted records never drain (a LeaseStore is required)`})
}

// A non-exclusive session never acquires a lease even when the runtime has a
// lease store, so the drainer a shared_outbox binding gets on it would skip every
// cycle: the runtime refuses the route.
func TestSharedOutbox_NonExclusiveBindingSessionIsRefused(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("non-exclusive-outbox"),
		goruntime.WithOutboxStore(NewFakeOutboxStore()), goruntime.WithLeaseStore(NewFakeLeaseStore()))
	addOutboxBindingRoute(t, rt, runsession.Config{SessionID: "dst-session"}, NewFakeSession(), NewFakeSender())
	requireValidationErrors(t, startForValidation(t, rt), []string{`route "outbox-route": shared_outbox invalid: ` +
		`binding session "dst-session" is non-exclusive; a non-exclusive session never acquires a lease, so ` +
		`its outbox drainer skips every cycle and persisted records never drain (make the session exclusive)`})
}

// A session has one manager, built from the first registration wiring reaches.
// A direct_hold route added earlier names the session as its own non-exclusive
// session block, so the drainer a later shared_outbox binding gets on that
// session runs on that non-exclusive manager and never holds a lease, even
// though the session sender itself is exclusive: the runtime refuses the route.
func TestSharedOutbox_BindingSessionManagedByAnEarlierNonExclusiveRouteIsRefused(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("first-wins-outbox"),
		goruntime.WithOutboxStore(NewFakeOutboxStore()), goruntime.WithLeaseStore(NewFakeLeaseStore()))
	sess, sender := NewFakeSession(), NewFakeSender()
	receive, recv, _ := helperQuiescentRoute("receive-route", nil)
	receiveCfg := runsession.Config{SessionID: "dst-session"}
	if err := rt.AddRoute(receive, recv, sender, sess, &receiveCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	addOutboxBindingRoute(t, rt, fastSessionConfig("dst-session"), sess, sender)
	requireValidationErrors(t, startForValidation(t, rt), []string{`route "outbox-route": shared_outbox invalid: ` +
		`binding session "dst-session" is managed under route "receive-route"'s non-exclusive session config; ` +
		`a non-exclusive session never acquires a lease, so its outbox drainer skips every cycle and persisted ` +
		`records never drain (make the session exclusive)`})
}

// The same session as an earlier shared_outbox route's exclusive session block
// and as a later route's binding is one lease-managed manager with one drainer,
// and the runtime accepts it.
func TestSharedOutbox_BindingSessionManagedByAnEarlierExclusiveRouteIsAccepted(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("first-wins-exclusive"),
		goruntime.WithOutboxStore(NewFakeOutboxStore()), goruntime.WithLeaseStore(NewFakeLeaseStore()))
	sess, sender := NewFakeSession(), NewFakeSender()
	sessCfg := fastSessionConfig("dst-session")
	primary := outboxRoute("primary-route", routing.DestinationBinding{ID: "b0", Address: "out/primary"})
	if err := rt.AddRoute(primary, NewFakeReceiver(), sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	addOutboxBindingRoute(t, rt, sessCfg, sess, sender)
	if err := startForValidation(t, rt); err != nil {
		t.Fatalf("Start refused a binding session that one exclusive, lease-managed manager drains: %v", err)
	}
}

// A shared_outbox route's own drainer runs on the session's one manager, too.
// When an earlier direct_hold route's non-exclusive session block names the same
// session, that manager is non-exclusive and the drainer never holds a lease,
// although the shared_outbox route's own session block is exclusive: the runtime
// refuses the route.
func TestSharedOutbox_OwnSessionManagedByAnEarlierNonExclusiveRouteIsRefused(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("first-wins-own-session"),
		goruntime.WithOutboxStore(NewFakeOutboxStore()), goruntime.WithLeaseStore(NewFakeLeaseStore()))
	sess, sender := NewFakeSession(), NewFakeSender()
	receive, recv, _ := helperQuiescentRoute("receive-route", nil)
	receiveCfg := runsession.Config{SessionID: "dst-session"}
	if err := rt.AddRoute(receive, recv, sender, sess, &receiveCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ownCfg := fastSessionConfig("dst-session")
	own := outboxRoute("outbox-route", routing.DestinationBinding{ID: "b1", Address: "out/addr"})
	if err := rt.AddRoute(own, NewFakeReceiver(), sender, sess, &ownCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	requireValidationErrors(t, startForValidation(t, rt), []string{`route "outbox-route": shared_outbox invalid: ` +
		`session "dst-session" is managed under route "receive-route"'s non-exclusive session config; ` +
		`a non-exclusive session never acquires a lease, so its outbox drainer skips every cycle and persisted ` +
		`records never drain (make the session exclusive)`})
}

// A shared_outbox route's own session block that is not lease-managed is
// refused once, by the route's own session rule, and not again for its drainer.
func TestSharedOutbox_OwnSessionNotLeaseManagedIsReportedOnce(t *testing.T) {
	cases := map[string]struct {
		lease   bool
		sessCfg runsession.Config
		want    string
	}{
		"non-exclusive with a lease store": {
			lease:   true,
			sessCfg: runsession.Config{SessionID: "dst-session"},
			want: `route "outbox-route": shared_outbox invalid: session is non-exclusive; a non-exclusive ` +
				`session never acquires a lease, so its outbox drainer skips every cycle and persisted records ` +
				`never drain (make the session exclusive)`,
		},
		"exclusive without a lease store": {
			sessCfg: fastSessionConfig("dst-session"),
			want:    `route "outbox-route": shared_outbox invalid: no LeaseStore configured for exclusive session`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := []goruntime.Option{goruntime.WithInstanceID("own-session-once"), goruntime.WithOutboxStore(NewFakeOutboxStore())}
			if tc.lease {
				opts = append(opts, goruntime.WithLeaseStore(NewFakeLeaseStore()))
			}
			rt := goruntime.New(opts...)
			own := outboxRoute("outbox-route", routing.DestinationBinding{ID: "b1", Address: "out/addr"})
			if err := rt.AddRoute(own, NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &tc.sessCfg); err != nil {
				t.Fatalf("AddRoute: %v", err)
			}
			requireValidationErrors(t, startForValidation(t, rt), []string{tc.want})
		})
	}
}
