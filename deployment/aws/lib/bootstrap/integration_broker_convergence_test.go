package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// TEST-3 (shipped composition root): broker-backed convergence
//
// The existing convergence_test.go pins the App's applied-but-not-converged
// state machine with a SENTINEL runtime (&goruntime.Runtime{}) — it never runs
// the actual runConvergenceWatch loop against a real MQTT session's readiness.
// This closes the finding's explicit gap ("the shipped AWS divergence is
// untested"): it drives the SHIPPED App.runConvergenceWatch against a runtime
// whose Exclusive MQTT session is broker-backed for real, proving:
//
//   - broker-unreachable  → the session never reaches LevelSubscribed → the
//     watch latches ConfigDegraded=1 with the "not converged" reason (a
//     committed reload that is NOT silently reported as success);
//   - broker-reachable    → the session reaches ServiceLevelFull ≥
//     LevelSubscribed → the watch clears/returns without ever marking degraded
//     (recovery / no false positive).
//
// Faithfulness: this is the same convergence code installPlan arms after every
// real reload; only the 60s budget floor is bypassed (runConvergenceWatch takes
// the budget as a parameter), so the session's connect/subscribe still run on
// real time against a real broker.
// ═══════════════════════════════════════════════════════════════════════════

// convNoopReceiver is a store-free source that never emits — the route exists
// only so the runtime manages (starts + health-tracks) the MQTT destination
// session, whose broker connection then drives runtime readiness.
type convNoopReceiver struct{}

func (convNoopReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// convDLQStore is a no-op DLQ so the shared-outbox route builds without pulling
// a store module into this package.
type convDLQStore struct{}

func (convDLQStore) Write(context.Context, routing.DLQEntry) error { return nil }
func (convDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (convDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, shared.ErrNotFound
}
func (convDLQStore) Delete(context.Context, []string) (int, error)                  { return 0, nil }
func (convDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) { return 0, nil }
func (convDLQStore) Purge(context.Context, time.Time) (int, error)                  { return 0, nil }

func convDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newConvergenceRuntime builds a started single-instance Exclusive bridge whose
// runtime-managed MQTT destination session points at brokerURL. On a reachable
// broker it reaches ServiceLevelFull; on an unreachable one it retries forever
// and stays at LevelRunning (< LevelSubscribed) — the broker-rejected shape.
func newConvergenceRuntime(t *testing.T, brokerURL string) *goruntime.Runtime {
	t.Helper()
	sessionID := mqttlocal.UniqueClientID("conv-sess")
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       mqttlocal.UniqueClientID("conv"),
		ConnectTimeout: 3 * time.Second,
		KeepAlive:      5,
		CleanStart:     true,
	}, connectivity.SessionExclusive, convDiscardLogger())
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	snd := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	rt := goruntime.New(
		goruntime.WithInstanceID("conv-"+sessionID),
		goruntime.WithLeaseStore(memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		goruntime.WithOutboxStore(memoryoutbox.NewStore()),
		goruntime.WithDLQStore(convDLQStore{}),
		goruntime.WithLogger(convDiscardLogger()),
	)
	sc := session.DefaultConfig(sessionID, true)
	sc.LeaseTTL = 5 * time.Second
	sc.RenewInterval = 400 * time.Millisecond
	sc.RenewCallTimeout = time.Second
	route := goruntime.RouteConfig{
		ID:       "conv-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "conv-bind", Address: "conv/out"}),
		Bindings: []routing.DestinationBinding{{ID: "conv-bind", SessionID: sessionID}},
	}
	require.NoError(t, rt.AddRoute(route, convNoopReceiver{}, snd, sess, &sc))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return rt
}

// TestReconfig1_ConvergenceWatch_BrokerUnreachable_MarksDegraded proves the
// shipped App marks applied-but-not-converged when a committed config's MQTT
// session cannot reach broker truth (readiness pinned below LevelSubscribed).
func TestReconfig1_ConvergenceWatch_BrokerUnreachable_MarksDegraded(t *testing.T) {
	mqttlocal.BrokerURL(t) // skip gate: -short / no Docker
	rt := newConvergenceRuntime(t, "tcp://127.0.0.1:1")

	require.Eventually(t, func() bool {
		return rt.ReadinessLevel(context.Background()) < ports.LevelSubscribed
	}, 15*time.Second, 200*time.Millisecond, "an unreachable-broker session must not reach LevelSubscribed")

	app := NewApp(testBootstrapCfg(), WithLogger(convDiscardLogger()))
	rec := &ports.RecordingExporter{}
	app.metricsExporter = rec
	app.runtimeRef.Set(rt)
	app.convergenceRt = rt

	// A REALISTIC budget (bypassing the 60s floor but comfortably ABOVE the ~2s a
	// healthy session takes to reach LevelSubscribed against a real broker): the
	// mark therefore means "did not converge in a window a healthy session would
	// have", not merely "budget shorter than one connect" — the watch polls for
	// the whole budget and marks only because readiness STAYS below LevelSubscribed
	// (the companion reachable test converges and clears within its budget). The
	// watch keeps polling after marking (never converges here), so run it in a
	// goroutine; the deferred cancel joins it even if an assertion below aborts.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	done := make(chan struct{})
	go func() { defer close(done); app.runConvergenceWatch(watchCtx, rt, 7, 3*time.Second) }()

	require.Eventually(t, func() bool {
		degraded, _ := app.degradedConfigWatch()
		return degraded
	}, 20*time.Second, 100*time.Millisecond, "budget expiry without convergence must latch ConfigDegraded")

	degraded, reason := app.degradedConfigWatch()
	require.True(t, degraded)
	require.Contains(t, reason, "not converged")
	require.Contains(t, reason, "config version 7")
	require.GreaterOrEqual(t, len(rec.FindEntries(shared.MetricConfigDegraded)), 1,
		"ConfigDegraded gauge must flip to 1 (the signal the shipped process previously lacked)")
	require.Less(t, rt.ReadinessLevel(context.Background()), ports.LevelSubscribed)

	watchCancel()
	<-done
}

// TestReconfig1_ConvergenceWatch_BrokerReachable_StaysConverged proves the
// watch does NOT cry wolf: a session that reaches LevelSubscribed against a real
// broker converges on the first poll and the watch returns without marking.
func TestReconfig1_ConvergenceWatch_BrokerReachable_StaysConverged(t *testing.T) {
	broker := mqttlocal.BrokerURL(t) // skip gate: -short / no Docker
	rt := newConvergenceRuntime(t, broker)

	require.Eventually(t, func() bool {
		return rt.ReadinessLevel(context.Background()) >= ports.LevelSubscribed
	}, 20*time.Second, 200*time.Millisecond, "a reachable-broker session must reach at least LevelSubscribed")

	app := NewApp(testBootstrapCfg(), WithLogger(convDiscardLogger()))
	rec := &ports.RecordingExporter{}
	app.metricsExporter = rec
	app.runtimeRef.Set(rt)
	app.convergenceRt = rt

	// A generous budget: the first poll observes LevelSubscribed and the watch
	// clears/returns without ever marking degraded.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app.runConvergenceWatch(ctx, rt, 8, 60*time.Second)

	degraded, _ := app.degradedConfigWatch()
	require.False(t, degraded, "a converged session must never be marked applied-but-not-converged")
	require.Empty(t, rec.FindEntries(shared.MetricConfigDegraded),
		"no ConfigDegraded gauge must be emitted for a healthy convergence")
}
