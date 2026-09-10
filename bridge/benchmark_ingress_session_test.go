package bridge

import (
	"context"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// The receiver-managed ingress session adds a pass over every receiver at the
// end of route wiring. These benchmarks bound that pass against the build it
// belongs to, on the shape it exists for and on a topology that mixes all three
// ways a session gets its manager, so the extra bookkeeping stays in the noise
// next to constructing the runtime — a build is a swap's outage window.

// BenchmarkBuilder_IngressSession_Simple builds one plan-driven receiver on its
// own session feeding a direct_hold route: the smallest shape the ingress
// session manages.
func BenchmarkBuilder_IngressSession_Simple(b *testing.B) {
	benchmarkBuild(b, ingressOnlyConfig("persistent"))
}

// BenchmarkBuilder_IngressSession_Mixed builds a topology where a third of the
// sessions are receiver-managed, a third are route-primary sessions and a third
// are binding-managed session senders, so the wiring pass has to skip the two
// existing paths correctly for every session it does not own.
func BenchmarkBuilder_IngressSession_Mixed(b *testing.B) {
	benchmarkBuild(b, mixedManagerConfig(20))
}

func benchmarkBuild(b *testing.B, cfg *ports.BridgeConfig) {
	b.Helper()
	sess := newCountingSession()
	tf := &ingressTransportFactory{capTransportFactory: capTransportFactory{caps: planDrivenCaps}, session: sess}
	for b.Loop() {
		rt, err := NewBuilder(cfg).
			RegisterTransportFactory("planned", tf).
			RegisterTransportFactory("sink", &fakeTransportFactory{}).
			RegisterStoreFactory("memory", &fakeStoreFactory{}).
			Build(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		// A built-but-never-started runtime still owns handles; release them.
		_ = rt.Stop(context.Background())
	}
}

// mixedManagerConfig declares n sessions of each kind: receiver-managed
// ingress sessions on direct_hold routes, route-primary sessions named by a
// session block on shared_outbox routes, and binding-managed sessions drained
// through a binding on shared_outbox routes.
func mixedManagerConfig(n int) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b-mixed"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Senders:  []ports.SenderDef{{ID: "sink", Transport: "sink"}},
		Bindings: []ports.BindingDef{{ID: "sink-b", SenderID: "sink", Address: "out/addr"}},
	}
	drop := ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}
	for i := 0; i < n; i++ {
		// Receiver-managed: the receiver is the only thing naming the session.
		in := fmt.Sprintf("in-%d", i)
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: in, Transport: "planned", SessionMode: "persistent"})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{
			ID: "rx-" + in, Transport: "planned", SessionID: in,
			Topics: []ports.SubscriptionDef{{Topic: in + "/#", QoS: 1}},
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: "r-" + in, ReceiverID: "rx-" + in, DeliveryMode: "direct_hold", Bindings: []string{"sink-b"},
			Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop", AllowUnfenced: true},
		})

		// Route-primary: a session block names the session and its sender.
		primary := fmt.Sprintf("p-%d", i)
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: primary, Transport: "planned", SessionMode: "exclusive"})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{
			ID: "rx-" + primary, Transport: "planned", SessionID: primary,
			Topics: []ports.SubscriptionDef{{Topic: primary + "/#", QoS: 1}},
		})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "tx-" + primary, Transport: "planned", SessionID: primary})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
			ID: "b-" + primary, SenderID: "tx-" + primary, SessionID: primary, Address: primary + "/out",
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: "r-" + primary, ReceiverID: "rx-" + primary, DeliveryMode: "shared_outbox",
			Bindings: []string{"b-" + primary}, Policy: drop,
			Session: &ports.RouteSessionDef{SessionID: primary, SenderID: "tx-" + primary},
		})

		// Binding-managed: a session-less source, a binding naming the session.
		dedicated := fmt.Sprintf("d-%d", i)
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: dedicated, Transport: "planned", SessionMode: "exclusive"})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: "rx-" + dedicated, Transport: "sink"})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "tx-" + dedicated, Transport: "planned", SessionID: dedicated})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
			ID: "b-" + dedicated, SenderID: "tx-" + dedicated, SessionID: dedicated, Address: dedicated + "/out",
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: "r-" + dedicated, ReceiverID: "rx-" + dedicated, DeliveryMode: "shared_outbox",
			Bindings: []string{"b-" + dedicated}, Policy: drop,
		})
	}
	return cfg
}
