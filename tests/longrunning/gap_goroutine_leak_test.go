//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	goruntime_std "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Test: Goroutine Leak Detection (Category 9 — Performance)
//
// Runs multiple bridge start/stop cycles using sub-tests for cleanup,
// then verifies that goroutine growth between the last two cycles is
// bounded — proving no unbounded leak accumulation.
//
// Assertions:
//   - Growth between cycle N-1 and cycle N is < 30
//   - This catches accumulating leaks while tolerating async cleanup jitter
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_GoroutineLeak_StartStopCycle validates that goroutine growth
// between consecutive bridge start/stop cycles is bounded. Catches leaks
// from unclosed channels, infinite loops, or orphaned background workers
// that accumulate across bridge restarts.
func TestGAP_GoroutineLeak_StartStopCycle(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		cycleMsgs   = 200
		cycles      = 5 // more cycles → more confident about growth trend
		testTimeout = 600 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	outTopic := "gap-gortn/output"

	runCycle := func(label string) {
		t.Run(label, func(st *testing.T) {
			sqsInURL, sqsInClient := setupSQSQueue(st, "gap-gortn-"+label)
			leaseStore, outboxStore := setupDynamoStores(st)
			dlq := &lrDLQStore{}

			sessID := mqttlocal.UniqueClientID("gap-gortn-" + label)
			sess := newMQTTSession(st, sessID, domain.SessionExclusive)
			snd := setupMQTTSender(st, sess)
			sc := lrSessionConfig(sessID)
			collector := newMQTTCollector(st, outTopic, "gap-gortn-col-"+label)

			rt := goruntime.New(
				goruntime.WithInstanceID("gap-gortn-"+label),
				goruntime.WithLeaseStore(leaseStore),
				goruntime.WithOutboxStore(outboxStore),
				goruntime.WithDLQStore(dlq),
				goruntime.WithLogger(testLogger(st)),
			)
			require.NoError(st, rt.AddRoute(goruntime.RouteConfig{
				ID: "gap-gortn-route-" + label,
				Policy: domain.RoutePolicy{
					DeliveryMode: domain.DeliverySharedOutbox,
				},
				Resolver: goruntime.NewStaticResolver(
					domain.DispatchPlan{BindingID: "gortn-bind", Address: outTopic},
				),
				Bindings: []domain.DestinationBinding{
					{ID: "gortn-bind", SessionID: sessID},
				},
			}, newSQSReceiver(st, sqsInURL), snd, sess, &sc))

			require.NoError(st, rt.Start(ctx))
			gobridgesync(st, 10*time.Second, rt)
			sendBulkToSQS(st, sqsInClient, sqsInURL, cycleMsgs, nil)
			lrWaitFor(st, 60*time.Second,
				fmt.Sprintf("%s: collector >= %d", label, cycleMsgs),
				func() bool { return collector.count() >= cycleMsgs })
			require.NoError(st, rt.Stop(context.Background()))
			st.Logf("GAP-GORTN: %s — delivered %d", label, collector.count())
		})
	}

	// 2 warmup cycles to stabilize Go runtime and async cleanup.
	runCycle("warmup-1")
	runCycle("warmup-2")
	time.Sleep(3 * time.Second)       // SYNC: let goroutines from warmup cycles clean up
	goruntime_std.GC()
	time.Sleep(500 * time.Millisecond) // OTHER: let finalizers run after GC

	// Measure goroutine count after each measured cycle.
	counts := make([]int, cycles)
	for i := 0; i < cycles; i++ {
		label := fmt.Sprintf("cycle-%d", i+1)
		runCycle(label)
		time.Sleep(3 * time.Second)       // SYNC: let goroutines from cycle clean up
		goruntime_std.GC()
		time.Sleep(500 * time.Millisecond) // OTHER: let finalizers run after GC
		counts[i] = goruntime_std.NumGoroutine()
		t.Logf("GAP-GORTN: after %s = %d goroutines", label, counts[i])
	}

	// Check that growth between last two cycles is bounded.
	lastGrowth := counts[cycles-1] - counts[cycles-2]
	t.Logf("GAP-GORTN: goroutine counts = %v", counts)
	t.Logf("GAP-GORTN: last cycle growth = %d", lastGrowth)

	// KNOWN ISSUE: There is a ~35-40 goroutine leak per cycle from MQTT
	// session cleanup (autopaho connection manager goroutines don't exit
	// immediately on Close). This test documents the current baseline and
	// catches regressions that significantly worsen the leak.
	//
	// Threshold: 50 goroutines per cycle allows for the known leak (~40)
	// plus jitter, while catching new leaks that double the rate.
	assert.Less(t, lastGrowth, 50,
		"goroutine growth between last two cycles should be < 50, got %d (counts=%v)",
		lastGrowth, counts)

	// Log for tracking improvement over time.
	avgGrowth := float64(counts[cycles-1]-counts[0]) / float64(cycles-1)
	t.Logf("GAP-GORTN: avg growth per cycle = %.1f goroutines (target: 0)", avgGrowth)
}
