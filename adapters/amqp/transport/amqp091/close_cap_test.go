// ═══════════════════════════════════════════════
// Adversarial-review remediation tests: closeConnAsync must not leak unbounded
// close goroutines under a sustained outage.
//
// Under a half-dead broker every reconnect attempt discards a connection whose
// Close can itself wedge waiting for connection.close-ok. A plain fire-and-forget
// `go conn.Close()` parks a new goroutine on every attempt and leaks unboundedly
// the exact outage-shape leak fixed, reintroduced on the close side.
// The fix bounds concurrent close goroutines (maxConcurrentCloses) and runs each
// under a deadline (conn.CloseDeadline) so a wedged close cannot be re-spawned
// without limit.
// ═══════════════════════════════════════════════
package amqp091

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_CloseConnAsync_CapsConcurrentCloses drives more wedged closes than
// the cap and asserts the number of close goroutines that actually enter Close
// stabilises at maxConcurrentCloses. Each mock's close hook blocks (broker never
// answers connection.close-ok), so once the cap is saturated further
// closeConnAsync calls must drop the connection without spawning another
// goroutine. Mutation (revert to an uncapped `go conn.Close()`): all M closes
// enter and `entered` stabilises at M > cap, failing the assertion.
func TestSession_CloseConnAsync_CapsConcurrentCloses(t *testing.T) {
	s := newResilienceSession(nil)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseClose) // unblock any still-parked close goroutines

	var entered atomic.Int64
	block := func() error {
		entered.Add(1)
		<-release // model a broker that never answers connection.close-ok
		return nil
	}

	const attempts = maxConcurrentCloses * 2
	for i := 0; i < attempts; i++ {
		mc := newMockConnection()
		// closeConnAsync uses CloseDeadline; wire CloseFn too so the mock blocks
		// whichever close path is taken.
		mc.CloseDeadlineFn = block
		mc.CloseFn = block
		s.closeConnAsync(mc)
	}

	got := wait.StableFor(t, func() int { return int(entered.Load()) },
		100*time.Millisecond, 3*time.Second)
	require.Equal(t, maxConcurrentCloses, got,
		"closeConnAsync must cap concurrent close goroutines at maxConcurrentCloses, "+
			"not spawn one per attempt (unbounded leak under a sustained outage)")

	// The cap must be reusable: once the parked closes finish, activeCloses must
	// drain back to 0 (proving both the spawn path and the dropped-attempt path
	// balance their activeCloses accounting) so the next outage cycle is not
	// permanently wedged at the cap.
	releaseClose()
	wait.Until(t, 2*time.Second, "activeCloses drains after the wedged closes finish", func() bool {
		return s.activeCloses.Load() == 0
	})
}
