package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════════
// Production contract: route readiness is PIPELINE LIVENESS, not recent
// delivery success.
//
// RouteHealth.Ready answers "is this route's runner and receiver up and able to
// accept work", NOT "did the last delivery reach its target". A context-honoring
// sender that fails 100% of deliveries therefore keeps a route Ready — the
// pipeline IS alive; the TARGET is not, and a readiness probe that flipped on
// target availability would eject the very instance that is correctly retrying
// and DLQ-ing.
//
// The accepted tradeoff is closed by a SEPARATE signal rather than by folding
// target health into readiness: every failed delivery raises
// shared.MetricRouteErrors tagged with the route, which is the delivery-stall
// series operators alarm on. This test pins both halves — readiness stays green,
// the stall is counted — so neither can be quietly changed.
// ═══════════════════════════════════════════════════════════════════════════

// TestDeepHealth_TotalDeliveryFailureKeepsRouteReady_ProductionContract proves
// the liveness-only semantics: after a delivery that never reaches the target,
// the route is Ready, the instance is ReadyForTraffic at LevelFull, and the
// failure is visible ONLY through the route-error counter.
func TestDeepHealth_TotalDeliveryFailureKeepsRouteReady_ProductionContract(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	dlqStore := NewFakeDLQStore()
	rt := newTestRuntime("bridge-liveness", nil, nil, dlqStore,
		goruntime.WithMetrics(metrics))

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	// A context-honoring target that refuses every delivery: transient, so the
	// route retries, caps, and DLQs — the pipeline itself never faults.
	sender.SetSendErr(shared.NewBridgeError("TARGET_DOWN", shared.ErrorTransient, "target refused"))

	const routeID = "liveness-route"
	cfg := goruntime.RouteConfig{
		ID: routeID,
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDLQ,
			// One replay attempt keeps the terminal decision immediate, so the
			// test asserts on a settled state rather than mid-backoff.
			MaxReplayAttempts: 1,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{{BindingID: "binding-1", Address: "devices/1/state"}},
		},
		Bindings:           []routing.DestinationBinding{{ID: "binding-1"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	require.NoError(t, rt.AddRoute(cfg, receiver, sender, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "route ready", func() bool {
		return routeReady(rt.DeepHealth(ctx), routeID)
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "stalled-1",
		Subject: "device.state.update",
		Payload: []byte("payload"),
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	// The delivery fails against the target and is settled by the route — either
	// handed back to the source for redelivery or written to the DLQ. Either way
	// it never reached the target.
	waitFor(t, 5*time.Second, "delivery stalled", func() bool {
		return len(metrics.FindEntries(shared.MetricRouteErrors)) >= 1
	})
	waitFor(t, 5*time.Second, "delivery settled", func() bool {
		return del.IsRetried() || del.IsAcked() || dlqStore.Count() >= 1
	})
	require.Zero(t, sender.SentCount(), "precondition: not a single delivery reached the target")

	dh := rt.DeepHealth(ctx)
	assert.True(t, routeReady(dh, routeID),
		"readiness is pipeline liveness: a live route whose TARGET is down must stay Ready")
	assert.False(t, routeHealth(dh, routeID).RouteDead,
		"a failing target is not a flapping route; RouteDead must stay false")
	assert.True(t, dh.ReadyForTraffic,
		"an instance that is correctly retrying and DLQ-ing must not eject itself from the load balancer")
	assert.Equal(t, ports.LevelFull, ports.ReadinessLevelFromDeepHealth(dh),
		"target availability must not cap the readiness level")

	// The delivery stall is observable — through the route-error series, which is
	// the signal operators alarm on, not through readiness.
	stall := metrics.FindEntries(shared.MetricRouteErrors)
	require.NotEmpty(t, stall, "a 100%-failing route must raise the delivery-stall counter")
	assert.True(t, metricHasTag(stall[0], shared.TagKeyRouteID, routeID),
		"the stall signal must name the route so an operator can locate it")
}

// routeHealth returns the RouteHealth projection for id, or the zero value.
func routeHealth(dh ports.DeepHealth, id string) ports.RouteHealth {
	for _, r := range dh.Routes {
		if r.ID == id {
			return r
		}
	}
	return ports.RouteHealth{}
}

func routeReady(dh ports.DeepHealth, id string) bool { return routeHealth(dh, id).Ready }
