//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC68: Soak Test
//
// Injects 100 msgs/sec through the Inject API for the configured duration,
// watching heap growth and goroutine stability throughout.
//
// TWO PROFILES, one test. The suite runs the SHORT profile (5 minutes) so
// `make test-long-running` stays usable; `make test-soak` sets
// GOBRIDGE_SOAK_DURATION=60m for the published hour, which is the interval a
// slow goroutine, timer, connection or memory leak needs to become visible. A
// short profile is a smoke test of the soak, not the soak.
//
// Assert: >= 95% delivered. Heap growth < 2x. Goroutine count stable.
// =========================================================================

const (
	// soakDurationEnv overrides the soak profile, e.g. "60m". Any duration
	// time.ParseDuration accepts.
	soakDurationEnv = "GOBRIDGE_SOAK_DURATION"
	// shortSoakDuration is the profile the ordinary suite runs.
	shortSoakDuration = 5 * time.Minute
	// soakDrainAllowance is the headroom added to the injection window for
	// delivery, teardown and the final assertions.
	soakDrainAllowance = 2 * time.Minute
)

// soakDuration reports the configured soak profile. An unparseable value fails
// the test rather than silently falling back: a run that believed it was doing
// the published hour and quietly did five minutes would be worse evidence than
// no run at all.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv(soakDurationEnv)
	if raw == "" {
		return shortSoakDuration
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a duration: %v", soakDurationEnv, raw, err)
	}
	if parsed <= 0 {
		t.Fatalf("%s=%q must be positive", soakDurationEnv, raw)
	}
	return parsed
}

func TestUC68_Soak(t *testing.T) {
	_ = withFreshInfra(t)
	duration := soakDuration(t)
	const (
		rate     = 100 // msgs/sec
		outTopic = "uc68/output"
	)
	testTimeout := duration + soakDrainAllowance
	t.Logf("UC68: soak profile %v (override with %s)", duration, soakDurationEnv)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Counting, not retaining: an hour at 100 msgs/sec is ~360,000 envelopes,
	// and a collector holding them all is hundreds of megabytes of the TEST's
	// heap — which the leak assertion below would read as the bridge leaking.
	// This test only ever counts.
	collector := newCountingMQTTCollector(t, outTopic, "uc68-col")

	sessID := mqttlocal.UniqueClientID("uc68-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc68-bridge"),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc68-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  200,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc68-bind", Address: outTopic},
		),
		SourceCapabilities: []ports.Capability{ports.CapHTTPEndpoint},
	}, &noopReceiver{}, snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	// Wait for the runtime to initialize (noopReceiver means no session sync).
	lrWaitFor(t, 5*time.Second, "bridge running", func() bool {
		return rt.DeepHealth(context.Background()).Running
	})

	heap := newHeapSampler(5 * time.Second)
	startGoroutines := runtime.NumGoroutine()

	// Inject at target rate for the soak duration.
	t.Logf("UC68: injecting %d msgs/sec for %v (expect ~%d msgs)",
		rate, duration, int(duration.Seconds())*rate)

	interval := time.Second / time.Duration(rate)
	injectCtx, injectCancel := context.WithTimeout(ctx, duration)
	defer injectCancel()

	injected := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

injectLoop:
	for {
		select {
		case <-injectCtx.Done():
			break injectLoop
		case <-ticker.C:
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      fmt.Sprintf("uc68-msg-%d", injected),
				Subject: outTopic,
				Payload: []byte(fmt.Sprintf(`{"seq":%d}`, injected)),
			})
			if err := rt.Inject(ctx, "uc68-route", env); err != nil {
				if isDebug() {
					t.Logf("UC68: inject %d failed: %v", injected, err)
				}
				continue
			}
			injected++
		}
	}

	t.Logf("UC68: injected %d messages, waiting for delivery", injected)

	// Wait for at least 95% delivery.
	target95 := int(float64(injected) * 0.95)
	lrWaitFor(t, 60*time.Second,
		fmt.Sprintf("collector >= %d (95%%)", target95),
		func() bool { return collector.count() >= target95 })

	heap.stop()
	endGoroutines := runtime.NumGoroutine()

	delivered := collector.count()
	deliveryPct := float64(delivered) / float64(injected) * 100

	t.Logf("UC68: injected=%d, delivered=%d (%.1f%%)", injected, delivered, deliveryPct)
	t.Logf("UC68: heap — initial=%dMB, max=%dMB, final=%dMB",
		heap.initialHeap()/(1<<20), heap.maxHeap()/(1<<20), heap.finalHeap()/(1<<20))
	t.Logf("UC68: goroutines — start=%d, end=%d, diff=%d",
		startGoroutines, endGoroutines, endGoroutines-startGoroutines)

	require.GreaterOrEqual(t, deliveryPct, 95.0,
		"At least 95%% of injected messages must be delivered")
	initialFloor := heap.initialHeap()
	if initialFloor < 50<<20 {
		initialFloor = 50 << 20 // floor at 50MB to avoid false positives when initial heap is tiny
	}
	require.Lessf(t, heap.finalHeap(), 2*initialFloor,
		"final heap %dMB must be <= 2x initial (floored at 50MB) after %d messages; "+
			"the collector retains nothing, so this heap is the bridge's",
		heap.finalHeap()/(1<<20), injected)

	goroutineDiff := endGoroutines - startGoroutines
	require.Less(t, goroutineDiff, 50,
		"Goroutine count must be stable (diff < 50)")
}

// =========================================================================
// UC67: Concurrent Reconcile During Message Flow
//
// Calls Reconcile 3 times mid-flow to toggle subscriptions while
// messages are being processed. Verifies no races or message loss.
// =========================================================================

func TestUC67_ConcurrentReconcile(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 3000
		outTopic    = "uc67/output"
		testTimeout = 240 * time.Second
	)

	sqsInURL, sqsInClient := setupSQSQueue(t, "uc67-in")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collector := newMQTTCollector(t, outTopic, "uc67-col")

	sessID := mqttlocal.UniqueClientID("uc67-sess")
	sess := setupMQTTSession(t, sessID, connectivity.SessionExclusive)
	snd := setupMQTTSender(t, sess)
	rx := newSQSReceiver(t, sqsInURL)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc67-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc67-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  100,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc67-bind", Address: outTopic},
		),
		SourceCapabilities: directHoldCaps,
	}, rx, snd, sess, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	t.Logf("UC67: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Trigger Reconcile 3 times at ~1000-msg intervals.
	reconcileTargets := []int{msgCount / 3, 2 * msgCount / 3, msgCount - 100}
	for i, target := range reconcileTargets {
		lrWaitFor(t, 120*time.Second,
			fmt.Sprintf("collector >= %d before reconcile %d", target, i+1),
			func() bool { return collector.count() >= target })

		t.Logf("UC67: reconcile %d at collector=%d", i+1, collector.count())
		err := sess.Reconcile(ctx, connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{
				{Topic: outTopic, QoS: 1},
			},
		})
		if err != nil {
			t.Logf("UC67: reconcile %d error (non-fatal): %v", i+1, err)
		}
	}

	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("unique >= %d", msgCount),
		func() bool { return countUnique(collector) >= msgCount })

	t.Logf("UC67: unique=%d, total=%d, dlq=%d",
		countUnique(collector), collector.count(), dlq.count())
	require.GreaterOrEqual(t, countUnique(collector), msgCount)
}
