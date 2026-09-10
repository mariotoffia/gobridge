package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
)

// TestSessionIngressQuiescenceWaiterCompletesWhenATargetBlackHoles pins the
// property a stateful source transport relies on when it drains ingress before
// recycling its broker connection: the drain ALWAYS completes in bounded time,
// because every accepted delivery is bounded by the route that owns it.
//
// This is what lets the MQTT settlement recovery drain under nothing tighter
// than its own attempt budget. The transport cannot invent a shorter deadline:
// a bound below a legitimate settlement classifies cooperative downstream
// slowness as an unrecoverable drain failure, which terminalizes the session
// and restarts every unrelated route in the process — one slow target becoming
// a whole-process restart loop.
//
// Here the target is worse than slow: it ignores its context entirely. The
// route's own send-wedge ceiling still settles the delivery, the waiter returns,
// and the sibling route on another session keeps delivering throughout.
func TestSessionIngressQuiescenceWaiterCompletesWhenATargetBlackHoles(t *testing.T) {
	blackHole := make(chan struct{})
	releaseBlackHole := closeOnce(blackHole)
	t.Cleanup(releaseBlackHole)

	rt := goruntime.New(goruntime.WithInstanceID("quiescence-black-hole"))

	// The wedged route: its sender ignores ctx and never returns on its own.
	wedgedCfg, wedgedRecv, wedgedSender := helperQuiescentRoute("wedged-route", blackHole)
	wedgedCfg.Policy.SendTimeout = 100 * time.Millisecond
	wedgedSess := NewFakeSession()
	wedgedSessCfg := runsession.Config{SessionID: "wedged-session"}
	require.NoError(t, rt.AddRoute(wedgedCfg, wedgedRecv, wedgedSender, wedgedSess, &wedgedSessCfg))

	// An unrelated route on its own session, with a healthy target.
	healthyCfg, healthyRecv, healthySender := helperQuiescentRoute("healthy-route", nil)
	healthySess := NewFakeSession()
	healthySessCfg := runsession.Config{SessionID: "healthy-session"}
	require.NoError(t, rt.AddRoute(healthyCfg, healthyRecv, healthySender, healthySess, &healthySessCfg))

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() {
		releaseBlackHole()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Stop(stopCtx)
	})

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	require.NoError(t, rt.WaitRouteReady(readyCtx, "wedged-route"))
	require.NoError(t, rt.WaitRouteReady(readyCtx, "healthy-route"))

	require.NoError(t, wedgedRecv.Emit(context.Background(),
		NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "wedged-1", Subject: "src"}))))

	// The drain runs with NO deadline of its own — exactly how the transport
	// invokes it once the adapter stops imposing a local bound.
	settled := make(chan error, 1)
	go func() { settled <- wedgedSess.WaitIngressQuiescent(context.Background()) }()

	select {
	case err := <-settled:
		require.NoError(t, err, "the drain must complete on the route's own wedge ceiling")
	case <-time.After(5 * time.Second):
		t.Fatal("ingress drain never completed against a context-ignoring target: " +
			"a source transport draining before a recycle would hang or be forced to invent a bound")
	}

	// Unrelated work is untouched: the sibling route still delivers.
	require.NoError(t, healthyRecv.Emit(context.Background(),
		NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "healthy-1", Subject: "src"}))))
	waitFor(t, 2*time.Second, "the unrelated route keeps delivering", func() bool {
		return healthySender.SentCount() == 1
	})

	healthyCtx, healthyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer healthyCancel()
	require.NoError(t, healthySess.WaitIngressQuiescent(healthyCtx),
		"a black-holed target on one session must not stall another session's drain")
}
