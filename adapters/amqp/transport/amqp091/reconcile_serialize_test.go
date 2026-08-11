// ═══════════════════════════════════════════════
// Production-readiness remediation test: reconcile serialization (MEDIUM).
//
// reconcile() has three drivers — Start, the public Reconcile, and the
// reconnect loop. Without a shared mutex a caller-driven Reconcile can
// interleave with a reconnect-driven reconcile; both open channels, declare
// topology and recompute activeSubs, risking a partially-applied plan. A
// dedicated reconcileMu now serialises them.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// gatedReconcileExporter blocks the first ReconcileLatency Timer call until
// release is closed, so a test can pin one reconcile inside its critical
// section while a second attempts to enter.
type gatedReconcileExporter struct {
	*ports.NoopExporter
	entered chan struct{}
	release chan struct{}
	timers  int32
}

func (g *gatedReconcileExporter) Timer(name string, _ time.Duration, _ ...shared.Tag) {
	if name != MetricAMQP091ReconcileLatency {
		return
	}
	if atomic.AddInt32(&g.timers, 1) == 1 {
		close(g.entered)
		<-g.release
	}
}

// TestSession_Reconcile_SerializedByMutex is the regression: while one
// reconcile holds reconcileMu a second reconcile must not proceed.
//
// Counterfactual (remove reconcileMu): the second reconcile runs
// concurrently and completes while the first is pinned, so wait.Silent
// observes bDone fire and fails.
func TestSession_Reconcile_SerializedByMutex(t *testing.T) {
	exp := &gatedReconcileExporter{
		NoopExporter: &ports.NoopExporter{},
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	sess.metrics = exp
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	emptyPlan := connectivity.SessionPlan{}

	aDone := make(chan error, 1)
	go func() { aDone <- sess.reconcile(context.Background(), mc, emptyPlan) }()

	// A is now inside reconcile, holding reconcileMu, blocked in Timer.
	wait.RequireClosed(t, exp.entered, 2*time.Second)

	bDone := make(chan error, 1)
	go func() { bDone <- sess.reconcile(context.Background(), mc, emptyPlan) }()

	// With the shared mutex B cannot complete while A holds it.
	wait.Silent(t, bDone, 100*time.Millisecond)

	// Release A; both reconciles then complete in series.
	close(exp.release)
	require.NoError(t, wait.RequireReceive(t, aDone, 2*time.Second))
	require.NoError(t, wait.RequireReceive(t, bDone, 2*time.Second))
}
