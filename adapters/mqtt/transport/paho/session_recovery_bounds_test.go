package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSessionRecovery_DrainOutlastsTheAdapterReconcileBound pins the split
// between the two phases of a settlement-recovery drain, which have different
// owners and therefore different ceilings:
//
//   - stopping the router from accepting new callbacks is ADAPTER teardown and
//     stays bounded by reconcile_timeout;
//   - waiting for deliveries the runtime already accepted to SETTLE is runtime
//     work, bounded by the route's own send-wedge, processor and store
//     ceilings, and may legitimately outlast any adapter-local bound.
//
// Counterfactual (the pre-fix five-second drain limit): a settlement that ran
// longer than five seconds — well inside the 30-second default send budget of
// any route — cancelled the drain, which the recovery classifies as
// unrecoverable. The session went terminal, the runtime restarted, and every
// unrelated route on that process restarted with it: one slow target produced a
// restart loop. The recycle metric below never fires and Reconcile reports the
// session permanently closed.
func TestSessionRecovery_DrainOutlastsTheAdapterReconcileBound(t *testing.T) {
	clk := clocktest.New()
	metrics := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://127.0.0.1:1883"},
		ClientID:   "recovery-slow-settlement",
		Clock:      clk,
	}, connectivity.SessionPersistent, nil, metrics)
	s.mu.Lock()
	s.cm = &fakeLiveConn{}
	s.connected = true
	s.mu.Unlock()

	var settleCtx context.Context
	settling := make(chan struct{})
	release := make(chan struct{})
	s.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		settleCtx = ctx
		close(settling)
		<-release
		// Report whether anything cancelled the settlement while it ran.
		return ctx.Err()
	})
	dialed := make(chan struct{}, 1)
	s.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dialed <- struct{}{}
		return &fakeLiveConn{}, func() {}, nil
	}

	require.NoError(t, s.requestRecovery(t.Context()))
	<-settling

	require.NotNil(t, settleCtx)
	_, hasAdapterDeadline := settleCtx.Deadline()
	require.False(t, hasAdapterDeadline,
		"the settlement phase must carry no adapter-local deadline: only the recovery attempt budget bounds it, "+
			"because the route's own send-wedge, processor and store ceilings are what bound a settlement")

	// A cooperative but slow downstream target keeps settling well past every
	// adapter-local bound.
	clk.Advance(s.reconcileTimeout() + time.Second)
	close(release)

	wait.Until(t, 2*time.Second, "the slow settlement completes and the connection is recycled", func() bool {
		return len(metrics.FindEntries(MetricMQTTSessionRecoveryRecycle)) == 1
	})
	<-dialed

	health := s.Health(t.Context())
	require.NoError(t, health.LastError, "cooperative slowness is not a session failure")
}

// TestSessionRecoveryAttemptTimeoutIsTheConfiguredSettlementRecoveryWait keeps
// the adapter's own recovery attempt and the route validator on one function: a
// validator that admitted a longer hold than the session waits for would let the
// recycle fail on a delivery the operator was told was safe.
func TestSessionRecoveryAttemptTimeoutIsTheConfiguredSettlementRecoveryWait(t *testing.T) {
	opts := SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"}, ClientID: "settlement-recovery-wait",
		ConnectTimeout: 7 * time.Second, ReconcileTimeout: 8 * time.Second, UnmatchedGrace: 9 * time.Second,
	}
	s := NewSession(opts, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { s.Router().shutdown() })

	want := (Config{Session: opts}).SettlementRecoveryWait(connectivity.SessionPersistent)
	if got := s.recoveryAttemptTimeout(); got != want {
		t.Fatalf("session recovery attempt timeout = %s, want the configured wait %s", got, want)
	}
}
