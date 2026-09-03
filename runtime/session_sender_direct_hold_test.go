package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// A plan-driven transport (MQTT, AMQP 0-9-1) subscribes only when a session
// manager reconciles the session's plan, and the builder accepts a binding
// that names the session as the way to get one ("or a binding targeting it").
// That registration must produce a manager for EVERY delivery mode: a
// direct_hold route's ingress session that is only ever managed under
// shared_outbox is a session that never connects, a receiver that never
// subscribes, and a bridge that reports ready while transporting nothing.
func TestSessionSender_DirectHoldRouteGetsItsSessionManaged(t *testing.T) {
	rt := newTestRuntime("bridge-direct-hold-session", nil, nil, nil)

	ingress := NewFakeSession()
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/#", QoS: 1}}}
	cfg := fastSessionConfig("ingress")
	cfg.Plan = plan
	if err := rt.RegisterSessionSender(cfg, ingress, NewFakeSender()); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}

	route := goruntime.RouteConfig{
		ID: "forward",
		Policy: routing.RoutePolicy{
			DeliveryMode:  routing.DeliveryDirectHold,
			DispatchMode:  routing.DispatchSingle,
			AllowUnfenced: true,
		},
		Bindings:           []routing.DestinationBinding{{ID: "to-archive", SessionID: "ingress", SenderID: "out", Address: "archive/sensors"}},
		SourceCapabilities: []ports.Capability{ports.CapSourceRedelivery},
	}
	if err := rt.AddRoute(route, NewFakeReceiver(), NewFakeSender(), ingress, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	waitFor(t, 5*time.Second, "ingress session started and its plan reconciled by a manager", func() bool {
		return ingress.IsStarted() && len(ingress.ReconciledPlans()) > 0
	})
	plans := ingress.ReconciledPlans()
	if len(plans[0].Subscriptions) != 1 || plans[0].Subscriptions[0].Topic != "sensors/#" {
		t.Fatalf("reconciled plan = %+v, want the receiver's subscription", plans[0])
	}
	// The session also carries the route's ingress: before it recycles a
	// broker connection it must wait for the deliveries the route already
	// accepted to settle, which only the installed waiter makes it do.
	if !ingress.HasIngressQuiescenceWaiter() {
		t.Fatal("binding-managed ingress session has no ingress-quiescence waiter; a reconnect would race in-flight deliveries")
	}
}
