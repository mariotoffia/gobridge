package bridge

import (
	"context"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// Benchmarks for the store-durability composition guard.
//
// Like the rollout barrier, this is CONTROL-PLANE work: it runs once per build
// and once per hot reload, never per message. The costs pinned here are the two
// that scale with the blueprint rather than with traffic:
//
//  1. the guard itself inside a full prepare — the price every start and every
//     accepted reload pays; and
//  2. the route-naming scan, which walks every route twice (outbox users, DLQ
//     users) to build the operator warning. A change that made that scan
//     allocate per route, or made it run on the durable path where it produces
//     nothing, would slow every reload of a large blueprint and no functional
//     test would notice.

// benchDurabilityConfig builds a blueprint of n outbox-backed routes, each with
// its own receiver, sender and binding, on separately typed lease and outbox
// stores so the durability postures can be varied independently.
func benchDurabilityConfig(n int) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "bench-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "lease-store"},
			Outbox: &ports.StoreConfig{Type: "outbox-store"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
	}

	for i := range n {
		id := fmt.Sprintf("%03d", i)
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: "rx-" + id, Transport: "sqs"})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "tx-" + id, Transport: "mqtt", SessionID: "mqtt-s1"})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
			ID: "b-" + id, SenderID: "tx-" + id, SessionID: "mqtt-s1", Address: "topic/" + id,
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID:           "r-" + id,
			ReceiverID:   "rx-" + id,
			DeliveryMode: "shared_outbox",
			Bindings:     []string{"b-" + id},
			Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "tx-" + id},
		})
	}

	return cfg
}

// BenchmarkStoreDurability_Prepare measures a whole prepare including the
// durability guard: the realistic per-reload cost an operator actually pays.
func BenchmarkStoreDurability_Prepare(b *testing.B) {
	for _, routes := range []int{1, 50, 500} {
		b.Run(fmt.Sprintf("routes=%d", routes), func(b *testing.B) {
			cfg := benchDurabilityConfig(routes)
			ctx := context.Background()

			b.ReportAllocs()
			for b.Loop() {
				builder := NewBuilder(cfg).
					RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
					RegisterTransportFactory("sqs", &fakeTransportFactory{}).
					RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: true}).
					RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: true})
				if _, err := builder.prepare(ctx); err != nil {
					b.Fatalf("prepare: %v", err)
				}
			}
		})
	}
}

// BenchmarkStoreDurability_RouteNaming isolates the warning's route scan, the
// only part of the guard whose cost grows with the blueprint.
func BenchmarkStoreDurability_RouteNaming(b *testing.B) {
	for _, routes := range []int{1, 50, 500} {
		b.Run(fmt.Sprintf("routes=%d", routes), func(b *testing.B) {
			cfg := benchDurabilityConfig(routes)

			b.ReportAllocs()
			for b.Loop() {
				_ = outboxRouteIDs(cfg)
				_ = dlqRouteIDs(cfg)
			}
		})
	}
}
