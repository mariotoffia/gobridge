package validate_test

import (
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// Baseline for the pre-commit validation path. Every admin config commit, every
// file reload and every startup runs the whole graph, and the retry/session
// bound checks added here run per route — so the cost scales with blueprint
// size, not with traffic.

// benchBlueprint builds a blueprint of n fully-specified routes: each carries
// the retry block and the session block whose fields the per-route rules walk,
// so the benchmark exercises the checks rather than skipping empty fields.
func benchBlueprint(n int) *ports.BridgeConfig {
	jitter := 0.2
	cfg := &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "bench", LogLevel: "info"},
		Sessions: []ports.SessionDef{{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"}},
		Stores: ports.StoresConfig{
			Outbox: &ports.StoreConfig{Type: "dynamodb"},
			Lease:  &ports.StoreConfig{Type: "dynamodb"},
		},
	}
	for i := range n {
		id := fmt.Sprintf("%d", i)
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: "rx" + id, Transport: "http"})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "tx" + id, Transport: "mqtt", SessionID: "s1"})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{ID: "b" + id, SenderID: "tx" + id, Address: "topic/" + id})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID:         "r" + id,
			ReceiverID: "rx" + id,
			Bindings:   []string{"b" + id},
			Policy: ports.PolicyDef{
				ReplayBudget: "15m",
				Backoff: ports.BackoffDef{
					InitialInterval: "1s",
					MaxInterval:     "30s",
					Multiplier:      2.0,
					Jitter:          &jitter,
				},
			},
			Session: &ports.RouteSessionDef{
				SessionID:            "s1",
				SenderID:             "tx" + id,
				LeaseTTL:             "45s",
				RenewInterval:        "10s",
				StepDownGrace:        "15s",
				BrokerHealthStepDown: "45s",
			},
		})
	}
	return cfg
}

// BenchmarkValidateBlueprintGraph measures a whole-blueprint validation at the
// sizes an operator actually deploys, from a single route to a large fleet
// blueprint.
func BenchmarkValidateBlueprintGraph(b *testing.B) {
	for _, routes := range []int{1, 10, 100} {
		cfg := benchBlueprint(routes)
		b.Run(fmt.Sprintf("routes=%d", routes), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if res := validate.ValidateBlueprintGraph(cfg); res != nil && res.HasErrors() {
					b.Fatalf("benchmark blueprint must be valid: %s", res.Error())
				}
			}
		})
	}
}

// BenchmarkValidateBlueprintGraph_Rejecting measures the failing path — an
// invalid retry policy on every route — because a rejected commit walks the
// same graph and additionally formats one message per violation.
func BenchmarkValidateBlueprintGraph_Rejecting(b *testing.B) {
	cfg := benchBlueprint(100)
	for i := range cfg.Routes {
		cfg.Routes[i].Policy.Backoff.Multiplier = 0.5
		cfg.Routes[i].Policy.Backoff.MaxInterval = "-30s"
	}
	b.ReportAllocs()
	for b.Loop() {
		if res := validate.ValidateBlueprintGraph(cfg); res == nil || !res.HasErrors() {
			b.Fatal("benchmark blueprint must be rejected")
		}
	}
}
