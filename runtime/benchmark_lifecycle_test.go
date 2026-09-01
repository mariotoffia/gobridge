package runtime_test

import (
	"context"
	"strconv"
	"testing"

	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// The shutdown path is the one every rolling restart, reload and scale-in pays
// for, and it grew work in this revision: routes now run on a detached context,
// so Stop is the only thing that cancels them and it publishes its result for
// every other caller. These benchmarks bound that cost — a regression here
// lengthens every SIGTERM, which is charged against the platform's kill budget.

// BenchmarkRuntimeStop_Idle is the floor: build, start and stop a runtime with
// no routes. Anything the lifecycle does unconditionally shows up here.
func BenchmarkRuntimeStop_Idle(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		rt := goruntime.New(goruntime.WithInstanceID("bench-idle"))
		if err := rt.Start(ctx); err != nil {
			b.Fatalf("Start: %v", err)
		}
		if err := rt.Stop(ctx); err != nil {
			b.Fatalf("Stop: %v", err)
		}
	}
}

// BenchmarkRuntimeStop_Routes is the realistic shape: several quiescent routes,
// each with a receiver, a sender and an unmanaged session that Stop has to
// close. It exercises the whole teardown — drain check, cancel, waitgroup join,
// session close, store close, metrics flush.
func BenchmarkRuntimeStop_Routes(b *testing.B) {
	for _, routes := range []int{1, 8, 32} {
		b.Run(strconv.Itoa(routes)+"-routes", func(b *testing.B) {
			ctx := context.Background()
			for b.Loop() {
				b.StopTimer()
				rt := goruntime.New(goruntime.WithInstanceID("bench-routes"))
				for i := range routes {
					cfg, recv, sender := helperQuiescentRoute("r"+strconv.Itoa(i), nil)
					if err := rt.AddRoute(cfg, recv, sender, NewFakeSession(), nil); err != nil {
						b.Fatalf("AddRoute: %v", err)
					}
				}
				if err := rt.Start(ctx); err != nil {
					b.Fatalf("Start: %v", err)
				}
				b.StartTimer()

				if err := rt.Stop(ctx); err != nil {
					b.Fatalf("Stop: %v", err)
				}
			}
		})
	}
}

// BenchmarkRuntimeStop_SecondCaller measures the path both shipped binaries take
// on SIGTERM: two callers race to Stop, one performs the teardown and the other
// blocks on it and reports its result. The second caller must be cheap — it is
// on the critical shutdown path, not a background nicety.
func BenchmarkRuntimeStop_SecondCaller(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		b.StopTimer()
		rt := goruntime.New(goruntime.WithInstanceID("bench-second-stop"))
		cfg, recv, sender := helperQuiescentRoute("r1", nil)
		if err := rt.AddRoute(cfg, recv, sender, NewFakeSession(), nil); err != nil {
			b.Fatalf("AddRoute: %v", err)
		}
		if err := rt.Start(ctx); err != nil {
			b.Fatalf("Start: %v", err)
		}
		if err := rt.Stop(ctx); err != nil {
			b.Fatalf("Stop: %v", err)
		}
		b.StartTimer()

		if err := rt.Stop(ctx); err != nil {
			b.Fatalf("second Stop: %v", err)
		}
	}
}
