package runtime_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
)

// A shared_outbox binding's session sender builds the session's manager before a
// later route's session block of the same id names another session object, so
// the removed-subscription dead-letter path goes on the object the manager runs.
func TestSessionRegistrations_DeadLetterPathGoesOnTheObjectTheManagerRuns(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("registrations-dead-letter"),
		goruntime.WithDLQStore(NewFakeDLQStore()), goruntime.WithOutboxStore(NewFakeOutboxStore()),
		goruntime.WithLeaseStore(NewFakeLeaseStore()))
	managed, other := newDeadLetterSession(), newDeadLetterSession()
	outbox := goruntime.RouteConfig{
		ID:       "outbox-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox, OnPermanentFailure: routing.FailureDrop, OnExpired: routing.ExpiredDrop},
		Bindings: []routing.DestinationBinding{{ID: "b1", Address: "out/addr", SessionID: "s1"}},
	}
	if err := rt.AddRoute(outbox, NewFakeReceiver(), NewFakeSender(), managed, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.RegisterSessionSender(fastSessionConfig("s1"), managed, NewFakeSender()); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	receive, recv, sender := helperQuiescentRoute("receive-route", nil)
	if err := rt.AddRoute(receive, recv, sender, other, &runsession.Config{SessionID: "s1"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	if got := managed.installs(); got != 1 {
		t.Fatalf("dead-letter path installed %d times on the object the manager runs, want 1", got)
	}
	if got := other.installs(); got != 0 {
		t.Fatalf("dead-letter path installed %d times on an object no manager runs, want 0", got)
	}
}

// A route's session block builds the session's manager before a session sender
// of the same id names another session object, and a route rides on that id, so
// the settlement barrier goes on the object the manager runs: that is the
// connection that recycles.
func TestSessionRegistrations_SettlementBarrierGoesOnTheObjectTheManagerRuns(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("registrations-barrier"))
	managed, other := NewFakeSession(), NewFakeSession()
	receive, recv, sender := helperQuiescentRoute("receive-route", nil)
	receive.SourceSessionID = "s1"
	if err := rt.AddRoute(receive, recv, sender, managed, &runsession.Config{SessionID: "s1"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.RegisterSessionSender(runsession.Config{SessionID: "s1"}, other, NewFakeSender()); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	startRuntime(t, rt)

	if !managed.HasIngressQuiescenceWaiter() {
		t.Fatal("the object the manager runs got no settlement barrier")
	}
	if other.HasIngressQuiescenceWaiter() {
		t.Fatal("an object no manager runs got the settlement barrier")
	}
}
