package bridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A reconfiguration swap is a full stop-and-rebuild, and it now derives a
// separate construction deadline after the old runtime's drain. These
// benchmarks bound the swap's own cost so that extra bookkeeping stays in the
// noise next to the build itself — a swap is a live outage window for every
// route it replaces.

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
