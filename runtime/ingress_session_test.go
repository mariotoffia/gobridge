package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// An ingress session is a session whose only job is to carry the subscriptions
// of the receivers bound to it: no route names it in a session block and no
// binding drains an outbox partition through it. A plan-driven transport (MQTT,
// AMQP 0-9-1) subscribes only when a manager reconciles the session plan, so
// such a session still needs a manager — one that starts the session,
// reconciles its plan and follows reconnects, and holds no lease.

func ingressSessionPlan() connectivity.SessionPlan {
	return connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/#", QoS: 1}}}
}

func ingressSessionConfig(sessionID string) session.Config {
	return session.Config{SessionID: sessionID, Plan: ingressSessionPlan()}
}

// directHoldRouteOn is a direct_hold route whose source rides on the named
// ingress session and whose destination needs no session at all.
func directHoldRouteOn(id string) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID: id,
		Policy: routing.RoutePolicy{
			DeliveryMode:  routing.DeliveryDirectHold,
			DispatchMode:  routing.DispatchSingle,
			AllowUnfenced: true,
		},
		Bindings:           []routing.DestinationBinding{{ID: "to-archive", SenderID: "out", Address: "archive/sensors"}},
		SourceCapabilities: []ports.Capability{ports.CapSourceRedelivery},
	}
}

func TestIngressSession_GetsAPlainManagerAndNoLease(t *testing.T) {
	// A lease store is present so the proof is that the session never takes a
	// lease, not that none could be taken.
	rt := newTestRuntime("bridge-ingress-session", nil, NewFakeLeaseStore(), nil)

	ingress := NewFakeSession()
	if err := rt.RegisterIngressSession(ingressSessionConfig("ingress"), ingress); err != nil {
		t.Fatalf("RegisterIngressSession: %v", err)
	}
	if err := rt.AddRoute(directHoldRouteOn("forward"), NewFakeReceiver(), NewFakeSender(), ingress, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	waitFor(t, 5*time.Second, "ingress session started and its plan reconciled", func() bool {
		return ingress.IsStarted() && ingress.PlanCount() > 0
	})
	plans := ingress.ReconciledPlans()
	if len(plans[0].Subscriptions) != 1 || plans[0].Subscriptions[0].Topic != "sensors/#" {
		t.Fatalf("reconciled plan = %+v, want the receiver's subscription", plans[0])
	}
	if got := ingress.StartCount(); got != 1 {
		t.Fatalf("the session was started %d times, want exactly one manager", got)
	}
	// No lease: the instance is standalone, not an owner or a standby, and the
	// session reports none.
	if role := rt.Role(); role != ports.RoleStandalone {
		t.Fatalf("role = %q, want %q: an ingress session takes no part in failover", role, ports.RoleStandalone)
	}
	if held, ok := rt.LeaseStatus()["ingress"]; !ok || held {
		t.Fatalf("lease status = (%v, %v), want the session listed and holding no lease", held, ok)
	}
	// It carries the route's ingress, so it gets the settlement barrier a
	// route-primary session gets before it recycles a broker connection.
	if !ingress.HasIngressQuiescenceWaiter() {
		t.Fatal("ingress session has no ingress-quiescence waiter; a reconnect would race in-flight deliveries")
	}

	// Route readiness closes on the route goroutine, independently of the
	// session manager's reconcile, so wait for the aggregate rather than assert
	// it off the reconcile alone.
	waitFor(t, 5*time.Second, "the runtime reports ready for traffic", func() bool {
		return rt.DeepHealth(context.Background()).ReadyForTraffic
	})
	dh := rt.DeepHealth(context.Background())
	var found *ports.SessionHealthDetail
	for i := range dh.Sessions {
		if dh.Sessions[i].SessionID == "ingress" {
			found = &dh.Sessions[i]
		}
	}
	if found == nil {
		t.Fatalf("deep health lists sessions %+v, want the ingress session among them", dh.Sessions)
	}
	if found.HasLease || found.ConnectAfterLease {
		t.Fatalf("deep health reports the ingress session as lease-bearing: %+v", *found)
	}

	if err := rt.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !ingress.IsClosed() {
		t.Fatal("Stop did not close the ingress session")
	}
	if got := ingress.CloseCount(); got != 1 {
		t.Fatalf("the ingress session was closed %d times, want once (by its manager, not again by the unmanaged-session sweep)", got)
	}
}

func TestIngressSession_NeverStartedRuntimeStillClosesIt(t *testing.T) {
	rt := newTestRuntime("bridge-ingress-unstarted", nil, nil, nil)
	ingress := NewFakeSession()
	if err := rt.RegisterIngressSession(ingressSessionConfig("ingress"), ingress); err != nil {
		t.Fatalf("RegisterIngressSession: %v", err)
	}
	if err := rt.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !ingress.IsClosed() {
		t.Fatal("a built-but-never-started runtime must release the ingress session it was handed")
	}
}

// A session has exactly one manager, so an ingress session may not also be a
// session sender or a route's primary session — and registration says so
// rather than leaving Start to pick one.
func TestIngressSession_RegistrationIsExclusive(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, rt *goruntime.Runtime) error
	}{
		{"empty id", func(_ *testing.T, rt *goruntime.Runtime) error {
			return rt.RegisterIngressSession(session.Config{}, NewFakeSession())
		}},
		{"nil session", func(_ *testing.T, rt *goruntime.Runtime) error {
			return rt.RegisterIngressSession(ingressSessionConfig("ingress"), nil)
		}},
		{"exclusive config", func(_ *testing.T, rt *goruntime.Runtime) error {
			cfg := ingressSessionConfig("ingress")
			cfg.Exclusive = true
			return rt.RegisterIngressSession(cfg, NewFakeSession())
		}},
		{"registered twice", func(t *testing.T, rt *goruntime.Runtime) error {
			if err := rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession()); err != nil {
				t.Fatalf("first registration: %v", err)
			}
			return rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession())
		}},
		{"already a session sender", func(t *testing.T, rt *goruntime.Runtime) error {
			if err := rt.RegisterSessionSender(fastSessionConfig("ingress"), NewFakeSession(), NewFakeSender()); err != nil {
				t.Fatalf("RegisterSessionSender: %v", err)
			}
			return rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession())
		}},
		{"then registered as a session sender", func(t *testing.T, rt *goruntime.Runtime) error {
			if err := rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession()); err != nil {
				t.Fatalf("RegisterIngressSession: %v", err)
			}
			return rt.RegisterSessionSender(fastSessionConfig("ingress"), NewFakeSession(), NewFakeSender())
		}},
		{"already a route primary session", func(t *testing.T, rt *goruntime.Runtime) error {
			primary := fastSessionConfig("ingress")
			if err := rt.AddRoute(directHoldRouteOn("r1"), NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &primary); err != nil {
				t.Fatalf("AddRoute: %v", err)
			}
			return rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession())
		}},
		{"then named as a route primary session", func(t *testing.T, rt *goruntime.Runtime) error {
			if err := rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession()); err != nil {
				t.Fatalf("RegisterIngressSession: %v", err)
			}
			primary := fastSessionConfig("ingress")
			return rt.AddRoute(directHoldRouteOn("r1"), NewFakeReceiver(), NewFakeSender(), NewFakeSession(), &primary)
		}},
		{"running runtime", func(t *testing.T, rt *goruntime.Runtime) error {
			if err := rt.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = rt.Stop(context.Background()) })
			return rt.RegisterIngressSession(ingressSessionConfig("ingress"), NewFakeSession())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTestRuntime("bridge-ingress-rules", nil, NewFakeLeaseStore(), nil)
			if err := tc.run(t, rt); err == nil {
				t.Fatal("registration succeeded, want a refusal")
			}
		})
	}
}
