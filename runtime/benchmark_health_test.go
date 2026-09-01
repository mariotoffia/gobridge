package runtime_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Health is the hottest read path a bridge serves: every orchestrator liveness
// and readiness probe, every load-balancer health check and every deep-health
// scrape lands here, at a cadence the operator sets and the process cannot
// refuse. DeepHealth now also decides whether the instance is EMPTY, so these
// benchmarks bound the sweep at the two shapes that matter — an instance
// carrying nothing, and one carrying a realistic number of routes.

// BenchmarkDeepHealth_EmptyRuntime is the start-empty floor: no routes, no
// sessions. It is the probe cost a process pays while it bridges nothing, which
// is exactly the state an operator probes hardest while they fix the config.
func BenchmarkDeepHealth_EmptyRuntime(b *testing.B) {
	ctx := context.Background()
	rt := goruntime.New(goruntime.WithInstanceID("bench-health-empty"))
	if err := rt.Start(ctx); err != nil {
		b.Fatalf("Start: %v", err)
	}
	b.Cleanup(func() { _ = rt.Stop(context.Background()) })

	for b.Loop() {
		if dh := rt.DeepHealth(ctx); !dh.Empty {
			b.Fatal("precondition: a runtime with no routes and no sessions must report empty")
		}
	}
}

// BenchmarkDeepHealth_Routes is the realistic shape: quiescent routes with a
// receiver, a sender and a session each, so the sweep walks every per-route and
// per-session projection a probe response is built from.
func BenchmarkDeepHealth_Routes(b *testing.B) {
	for _, routes := range []int{1, 8, 32} {
		b.Run(strconv.Itoa(routes)+"-routes", func(b *testing.B) {
			ctx := context.Background()
			rt := goruntime.New(goruntime.WithInstanceID("bench-health-routes"))
			for i := range routes {
				cfg, recv, sender := helperQuiescentRoute("r"+strconv.Itoa(i), nil)
				if err := rt.AddRoute(cfg, recv, sender, NewFakeSession(), nil); err != nil {
					b.Fatalf("AddRoute: %v", err)
				}
			}
			if err := rt.Start(ctx); err != nil {
				b.Fatalf("Start: %v", err)
			}
			b.Cleanup(func() { _ = rt.Stop(context.Background()) })

			for b.Loop() {
				if dh := rt.DeepHealth(ctx); dh.Empty {
					b.Fatal("precondition: a runtime carrying routes must not report empty")
				}
			}
		})
	}
}

// BenchmarkReadinessLevelFromDeepHealth bounds the pure snapshot→level
// derivation both handlers call. It runs once per probe on top of the sweep
// above, and the empty case short-circuits before the session fold — a shape
// worth keeping visible so the cap cannot silently become the expensive path.
func BenchmarkReadinessLevelFromDeepHealth(b *testing.B) {
	satisfied := true
	populated := ports.DeepHealth{Running: true, Healthy: true, ReadyForTraffic: true}
	for i := range 32 {
		populated.Sessions = append(populated.Sessions, ports.SessionHealthDetail{
			SessionID:              "sess-" + strconv.Itoa(i),
			Connected:              true,
			SubscriptionsSatisfied: &satisfied,
			ServiceLevel:           ports.ServiceLevelFull,
			Ready:                  true,
		})
		populated.Routes = append(populated.Routes, ports.RouteHealth{ID: "r" + strconv.Itoa(i), Ready: true})
	}

	b.Run("empty", func(b *testing.B) {
		dh := ports.DeepHealth{Running: true, Healthy: true, Empty: true}
		for b.Loop() {
			if ports.ReadinessLevelFromDeepHealth(dh) != ports.LevelRunning {
				b.Fatal("an empty instance must classify as running")
			}
		}
	})

	b.Run("32-sessions-32-routes", func(b *testing.B) {
		for b.Loop() {
			if ports.ReadinessLevelFromDeepHealth(populated) != ports.LevelFull {
				b.Fatal("a fully converged instance must classify as full")
			}
		}
	})
}
