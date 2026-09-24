package bridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A full reconfiguration swap is a stop-and-rebuild, and it derives a separate
// construction deadline after the old runtime's drain. These benchmarks bound
// the swap's own cost so that extra bookkeeping stays in the noise next to the
// build itself — a swap is a live outage window for every route it replaces.

// BenchmarkSupervisorSwap_Overlap measures the overlap path: build the
// replacement, stop the old runtime, start the replacement.
func BenchmarkSupervisorSwap_Overlap(b *testing.B) {
	benchmarkSwap(b, SwapOverlap)
}

// BenchmarkSupervisorSwap_PrepareCommit measures the exclusive-identity path:
// prepare, stop the old runtime, then complete and start — the one that now
// arms its construction deadline after the drain instead of before it.
func BenchmarkSupervisorSwap_PrepareCommit(b *testing.B) {
	benchmarkSwap(b, SwapPrepareCommit)
}

// BenchmarkSupervisorReload_InPlaceOneOfManyUnits measures a reload that
// changes one owner's route of many under SwapAuto: only that owner's reload
// unit is retired and rebuilt while the runtime and every other unit run on.
//
// Read it against the full replacement below as the Supervisor's own overhead,
// not as the operator-visible gain. The transports are in-memory fakes, so a
// full rebuild costs almost nothing here, while the in-place plan takes the
// content identity of every unit in both documents. In production each session
// the full replacement reconnects — dial, TLS, CONNECT, subscription reconcile —
// dwarfs that planning, and the in-place reload skips all but one.
func BenchmarkSupervisorReload_InPlaceOneOfManyUnits(b *testing.B) {
	benchmarkReloadOneOfManyUnits(b, SwapAuto, SwapInPlace)
}

// BenchmarkSupervisorReload_FullReplacementOfManyUnits measures the same reload
// under an explicit SwapOverlap, which rebuilds every unit.
func BenchmarkSupervisorReload_FullReplacementOfManyUnits(b *testing.B) {
	benchmarkReloadOneOfManyUnits(b, SwapOverlap, SwapOverlap)
}

func benchmarkReloadOneOfManyUnits(b *testing.B, mode, want SwapMode) {
	b.Helper()
	const owners = 20
	names := make([]string, owners)
	for i := range names {
		names[i] = "owner-" + strconv.Itoa(i)
	}

	onSwap, swaps := swapChan(1)
	s := NewSupervisor(WithSwapMode(mode), WithOnSwap(onSwap))
	s.RegisterTransport("tracked", newPerSessionTransportFactory(false))
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	changes := make(chan *ports.BridgeConfig, 1)
	_ = runSupervisorAsync(ctx, s, applyTestConfig(names...), changes)
	waitForRuntime(s, 5*time.Second)

	i := 0
	for b.Loop() {
		i++
		next := applyTestConfig(names...)
		next.Version = i + 1
		next.Routes[owners-1].Policy.MaxInFlight = 1 + i%2 // each reload changes the last owner's route
		if !sendConfig(changes, next, 5*time.Second) {
			b.Fatal("config channel did not accept the candidate")
		}
		select {
		case ev := <-swaps:
			if ev.Error != nil || ev.SwapMode != want {
				b.Fatalf("reload: mode %d, error %v; want mode %d", ev.SwapMode, ev.Error, want)
			}
		case <-time.After(10 * time.Second):
			b.Fatal("reload did not complete within the benchmark deadline")
		}
	}
}

func benchmarkSwap(b *testing.B, mode SwapMode) {
	b.Helper()

	s := newTestSupervisor(WithSwapMode(mode))
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)

	changes := make(chan *ports.BridgeConfig, 1)
	_ = runSupervisorAsync(ctx, s, supervisorTestConfig("bench-0"), changes)
	waitForRuntime(s, 5*time.Second)

	i := 0
	for b.Loop() {
		i++
		cfg := supervisorTestConfig("bench-" + strconv.Itoa(i))
		if !sendConfig(changes, cfg, 5*time.Second) {
			b.Fatal("config channel did not accept the candidate")
		}
		// The swap runs on the supervisor goroutine; wait for it to become the
		// running config before timing the next one, so each iteration measures
		// exactly one complete swap.
		if !wait.Poll(10*time.Second, func() bool {
			running := s.Config()
			return running != nil && len(running.Routes) > 0 && running.Routes[0].ID == cfg.Routes[0].ID
		}) {
			b.Fatal("swap did not complete within the benchmark deadline")
		}
	}
}
