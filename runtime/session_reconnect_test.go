package runtime_test

// ═══════════════════════════════════════════════════════════════════════
// S9 — Reconnect Reconcile Errors: propagation tests
//
// When a sess reconnects and Reconcile fails (ACL change, topic
// deletion), the SessionManager must log the error, emit a metric,
// and propagate the failure so the bridge can take corrective action.
//
//   ┌──────────┐  SessionConnected  ┌───────────────┐  error  ┌────────────┐
//   │  Session │───────────────────▶│  Reconcile()  │────────▶│ propagate  │
//   └──────────┘                    └───────────────┘         └────────────┘
//                                          │                       │
//                                          ▼                       ▼
//                                   MetricReconcileFailures   Run() returns err
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────┬──────┐
// │ ID   │ Description                                          │ Type │
// ├──────┼──────────────────────────────────────────────────────┼──────┤
// │ T001 │ Reconnect + Reconcile fail → error logged, Run exits │ unit │
// │ T002 │ Reconnect + Reconcile fail → metric emitted          │ unit │
// │ T003 │ Reconnect + Reconcile OK → no error, no metric       │ unit │
// │ T004 │ RenewLoop + Reconcile fail → renewLoop exits         │ unit │
// └──────┴──────────────────────────────────────────────────────┴──────┘
// ═══════════════════════════════════════════════════════════════════════

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSessionManager_ReconnectReconcileError_LogsAndPropagates validates
// that when Reconcile fails on a reconnect (second SessionConnected),
// the error propagates from Run and the manager exits.
//
// Timeline:
// ───────────────────────────────────────────────────────────────
//
//	T0: Start sess (non-exclusive), initial Reconcile OK
//	T1: Push SessionConnected (reconnect), ReconcileErr set
//	T2: handleSessionEvent returns error → handleEvents exits
//	T3: Run returns the reconcile error
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns non-nil error
//   - Error message contains "reconcile"
func TestSessionManager_ReconnectReconcileError_LogsAndPropagates(t *testing.T) {
	sess := NewFakeSession()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID: "sess-recon-err",
			Exclusive: false,
		},
		sess, nil, "bridge-1", slog.Default(),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Run(context.Background())
	}()

	wait.Until(t, 2*time.Second, "Run reaches event loop",
		func() bool { return sess.PlanCount() > 0 })

	sess.SetReconcileErr(errors.New("ACL denied topic"))

	sess.PushEvent(ports.SessionEvent{
		Type:      ports.SessionConnected,
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected Run to return error on reconcile failure")
		}
		if !strings.Contains(err.Error(), "reconcile") {
			t.Fatalf("expected error to mention reconcile, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run should exit after reconcile failure on reconnect")
	}
}

// TestSessionManager_ReconnectReconcileError_EmitsMetric validates that
// MetricReconcileFailures is emitted when Reconcile fails on reconnect.
//
// Timeline:
// ───────────────────────────────────────────────────────────────
//
//	T0: Start sess, initial Reconcile OK
//	T1: Push SessionConnected, ReconcileErr set
//	T2: MetricReconcileFailures counter incremented
//	T3: Run exits with error
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - MetricReconcileFailures counter == 1
//   - Counter has session_id tag
func TestSessionManager_ReconnectReconcileError_EmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewFakeSession()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID: "sess-recon-metric",
			Exclusive: false,
		},
		sess, nil, "bridge-1", nil,
	)
	mgr.SetMetrics(rec)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Run(context.Background())
	}()

	wait.Until(t, 2*time.Second, "Run reaches event loop",
		func() bool { return sess.PlanCount() > 0 })

	sess.SetReconcileErr(errors.New("topic deleted"))

	sess.PushEvent(ports.SessionEvent{
		Type:      ports.SessionConnected,
		Timestamp: time.Now(),
	})

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Run should exit after reconcile failure")
	}

	entries := rec.FindEntries(shared.MetricReconcileFailures)
	if len(entries) == 0 {
		t.Fatal("expected MetricReconcileFailures counter emission")
	}

	found := false
	for _, tag := range entries[0].Tags {
		if tag.Key == shared.TagKeySessionID && tag.Value == "sess-recon-metric" {
			found = true
		}
	}
	if !found {
		t.Error("expected session_id tag on MetricReconcileFailures")
	}
}

// TestSessionManager_ReconnectReconcileOK_NoError validates that when
// Reconcile succeeds on reconnect, no error propagates and no failure
// metric is emitted.
//
// Timeline:
// ───────────────────────────────────────────────────────────────
//
//	T0: Start sess, initial Reconcile OK
//	T1: Push SessionConnected (reconnect), Reconcile OK
//	T2: Manager continues running
//	T3: Context cancelled → Run returns ctx.Err
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns context.Canceled (not a reconcile error)
//   - MetricReconcileFailures not emitted
//   - MQTTReconnects counter == 1
func TestSessionManager_ReconnectReconcileOK_NoError(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sess := NewFakeSession()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID: "sess-recon-ok",
			Exclusive: false,
		},
		sess, nil, "bridge-1", nil,
	)
	mgr.SetMetrics(rec)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Run(ctx)
	}()

	wait.Until(t, 2*time.Second, "initial Reconcile called",
		func() bool { return sess.PlanCount() >= 1 })

	sess.PushEvent(ports.SessionEvent{
		Type:      ports.SessionConnected,
		Timestamp: time.Now(),
	})
	wait.Until(t, 2*time.Second, "first reconnect Reconcile called",
		func() bool { return sess.PlanCount() >= 2 })

	sess.PushEvent(ports.SessionEvent{
		Type:      ports.SessionConnected,
		Timestamp: time.Now(),
	})
	wait.Until(t, 2*time.Second, "second reconnect Reconcile called",
		func() bool { return sess.PlanCount() >= 3 })

	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled or nil, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run should exit after context cancellation")
	}

	failEntries := rec.FindEntries(shared.MetricReconcileFailures)
	if len(failEntries) != 0 {
		t.Fatalf("expected 0 MetricReconcileFailures, got %d", len(failEntries))
	}

	reconnects := rec.FindEntries(shared.MetricMQTTReconnects)
	if len(reconnects) != 1 {
		t.Fatalf("expected 1 MQTTReconnects (second connect counts), got %d", len(reconnects))
	}
}

// TestSessionManager_RenewLoop_ReconnectReconcileError_Exits validates
// that during an exclusive sess's renewLoop, a reconnect with a
// failing Reconcile causes the renewLoop to exit with the error.
//
// Timeline:
// ───────────────────────────────────────────────────────────────
//
//	T0: Start exclusive sess, lease acquired, Reconcile OK
//	T1: Enter renewLoop
//	T2: Push SessionConnected, ReconcileErr set
//	T3: renewLoop receives error from handleSessionEvent
//	T4: Run re-acquires lease and hits Reconcile error again → exits
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Run returns non-nil error containing "reconcile"
func TestSessionManager_RenewLoop_ReconnectReconcileError_Exits(t *testing.T) {
	sess := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	mgr := session.NewFromConfig(
		session.Config{
			SessionID:     "sess-renew-recon",
			Exclusive:     true,
			LeaseTTL:      5 * time.Second,
			RenewInterval: 100 * time.Millisecond,
			RenewJitter:   0,
			MaxRenewFails: 3,
			StepDownGrace: 50 * time.Millisecond,
		},
		sess, leaseStore, "bridge-1", nil,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Run(context.Background())
	}()

	wait.Until(t, 2*time.Second, "exclusive sess acquired lease and reconciled",
		func() bool { return sess.PlanCount() > 0 })

	sess.SetReconcileErr(errors.New("subscription denied after reconnect"))

	sess.PushEvent(ports.SessionEvent{
		Type:      ports.SessionConnected,
		Timestamp: time.Now(),
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected Run to return error when renewLoop reconcile fails")
		}
		if !strings.Contains(err.Error(), "subscription denied") {
			t.Fatalf("expected error to contain original reconcile failure, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run should exit after reconcile failure in renewLoop")
	}
}
