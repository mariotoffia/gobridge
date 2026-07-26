//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	goruntime_std "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Test: Goroutine Leak Detection (Category 9 — Performance)  [TEST-5]
//
// Runs many bridge start/stop cycles, then proves the goroutine count DRAINS
// back toward the baseline within a bounded window — eventual near-zero growth,
// not merely "not worse than an accepted per-cycle leak".
//
// Why "wait for the count to descend to baseline+tolerance" (and not a fixed
// settle + last-cycle delta, nor a floor-detector): autopaho's connection-
// manager goroutines do not exit immediately on Close — they unwind in MULTIPLE
// stages with intermediate plateaus up to ~15s. Measured decay after 3 cycles:
//
//	t=  0s  n=126        ← peak just after the cycles (flat plateau ~20s)
//	t= 35s  n= 58        ← intermediate plateau (~15s)
//	t= 60s  n=  8
//	t= 65s  n=  2        ← fully drained
//
// The previous test sampled at a fixed 3.5s — deep inside the first plateau —
// and recorded ~33 "leaked" goroutines/cycle that were still draining, then
// passed on a soft last-two-cycle delta < 50. Because the drain has multiple
// plateaus, "the settled floor" cannot be identified reliably; but a TARGET the
// count must descend to can, since it sails through every plateau above it.
// tolerance is generous (30) because the drained floor sits ~12 above the cold
// baseline (the AWS SDK HTTP pools the cycles open keep idle goroutines alive);
// a genuine leak keeps the floor far above the target and fails HARD via the
// wait timeout.
//
// Assertion:
//   - Within gortnDrainBudget, NumGoroutine() descends to ≤ baseline + gortnTolerance.
// ═══════════════════════════════════════════════════════════════════════════

const (
	gortnCycleMsgs = 200
	gortnCycles    = 5
	// gortnDrainBudget bounds how long the async autopaho unwind may take.
	// Empirically full unwind is ~60s after Close (see decay above); 180s is
	// generous headroom, and a real leak (a floor that never descends to the
	// threshold) still trips the assertion within the window.
	gortnDrainBudget = 180 * time.Second
	// gortnTolerance: the drained goroutine count must return to within this of
	// the pre-bridge baseline. The measured floor sits ~12 goroutines ABOVE the
	// cold baseline because the AWS SDK HTTP pools + shared broker connection the
	// cycles initialise keep idle-connection goroutines alive (governed by their
	// idle timeout, not GC). 30 covers that residue with wide margin on slower/
	// higher-GOMAXPROCS runners while still failing a genuine per-cycle leak
	// (≥~4/cycle ⇒ ≥~20 over the run ⇒ a floor that never descends to the
	// threshold within the budget). A tighter, warm-baseline gate is defeated
	// here by the drain's shape: autopaho unwinds in MULTIPLE stages with
	// intermediate plateaus up to ~15s, so "the settled floor" cannot be
	// identified reliably — but a target the count must DESCEND to can, because
	// it passes straight through every plateau that sits above it.
	gortnTolerance   = 30
	gortnTestTimeout = 600 * time.Second
)

// TestGAP_GoroutineLeak_StartStopCycle validates that repeated bridge
// start/stop cycles leave NO net goroutines once asynchronous cleanup has
// completed — the goroutine count returns to the pre-bridge baseline within a
// bounded drain budget. Catches leaks from unclosed channels, infinite loops,
// or orphaned background workers that would accumulate across restarts.
func TestGAP_GoroutineLeak_StartStopCycle(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), gortnTestTimeout)
	defer cancel()

	outTopic := "gap-gortn/output"

	runCycle := func(label string) {
		t.Run(label, func(st *testing.T) {
			sqsInURL, sqsInClient := setupSQSQueue(st, "gap-gortn-"+label)
			leaseStore, outboxStore := setupDynamoStores(st)
			dlq := &lrDLQStore{}

			sessID := mqttlocal.UniqueClientID("gap-gortn-" + label)
			sess := newMQTTSession(st, sessID, connectivity.SessionExclusive)
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
				Policy: routing.RoutePolicy{
					DeliveryMode: routing.DeliverySharedOutbox,
				},
				Resolver: goruntime.NewStaticResolver(
					routing.DispatchPlan{BindingID: "gortn-bind", Address: outTopic},
				),
				Bindings: []routing.DestinationBinding{
					{ID: "gortn-bind", SessionID: sessID},
				},
			}, newSQSReceiver(st, sqsInURL), snd, sess, &sc))

			require.NoError(st, rt.Start(ctx))
			gobridgesync(st, 10*time.Second, rt)
			sendBulkToSQS(st, sqsInClient, sqsInURL, gortnCycleMsgs, nil)
			lrWaitFor(st, 60*time.Second,
				fmt.Sprintf("%s: collector >= %d", label, gortnCycleMsgs),
				func() bool { return collector.count() >= gortnCycleMsgs })
			require.NoError(st, rt.Stop(context.Background()))
			st.Logf("GAP-GORTN: %s — delivered %d", label, collector.count())
		})
	}

	// Baseline: goroutine count with NO bridge running, before any cycle.
	goruntime_std.GC()
	baseline := goruntime_std.NumGoroutine()
	t.Logf("GAP-GORTN: baseline (no bridge) = %d goroutines", baseline)

	for i := 0; i < gortnCycles; i++ {
		runCycle(fmt.Sprintf("cycle-%d", i+1))
	}
	peak := goruntime_std.NumGoroutine()

	// STRICT: wait for the count to DESCEND back to baseline+tolerance. wait.Until
	// waits THROUGH the multi-stage autopaho unwind (the count sits above the
	// threshold at every intermediate plateau, so it keeps polling) and fails
	// HARD via its timeout if a genuine per-cycle leak keeps the floor above the
	// threshold forever — instead of the old "log the growth and pass".
	final := peak
	wait.Until(t, gortnDrainBudget, "goroutines drain back toward baseline after cleanup", func() bool {
		goruntime_std.GC()
		final = goruntime_std.NumGoroutine()
		return final <= baseline+gortnTolerance
	})

	t.Logf("GAP-GORTN: baseline=%d peak=%d settled=%d (tolerance=%d) after %d start/stop cycles",
		baseline, peak, final, gortnTolerance, gortnCycles)
	require.LessOrEqualf(t, final, baseline+gortnTolerance,
		"goroutine count did not drain to baseline after %d cycles + %s cleanup: "+
			"settled=%d, baseline=%d (net leak of %d — unclosed channels or orphaned workers?)",
		gortnCycles, gortnDrainBudget, final, baseline, final-baseline)
}
