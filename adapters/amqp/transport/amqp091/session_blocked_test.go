// ═══════════════════════════════════════════════
// Production-readiness remediation tests: broker flow-control observation.
//
// Covers Finding #9 — the session observes connection.blocked /
// connection.unblocked notifications (RabbitMQ resource alarms) and
// reflects them into Health (ServiceLevel + LastError), a metric, and a
// log line, instead of letting a blocked broker masquerade as a string
// of send timeouts.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// recordingExporter is a minimal thread-safe MetricsExporter that counts
// Counter invocations so tests can assert a metric was emitted.
type recordingExporter struct {
	mu       sync.Mutex
	counters map[string]int64
}

func (r *recordingExporter) Counter(name string, value int64, _ ...shared.Tag) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters == nil {
		r.counters = make(map[string]int64)
	}
	r.counters[name] += value
}

func (r *recordingExporter) Gauge(string, float64, ...shared.Tag)       {}
func (r *recordingExporter) Histogram(string, float64, ...shared.Tag)   {}
func (r *recordingExporter) Timer(string, time.Duration, ...shared.Tag) {}
func (r *recordingExporter) Flush(context.Context) error                { return nil }
func (r *recordingExporter) Close(context.Context) error                { return nil }

func (r *recordingExporter) count(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[name]
}

// TestSession_BlockedWatcher_DegradesAndRecovers drives the blocked
// watcher through a block -> unblock cycle and asserts the observable
// effects on Health and metrics. Determinism comes from pushing
// notifications on the mock's blocked channel and polling Health (no
// sleeps).
func TestSession_BlockedWatcher_DegradesAndRecovers(t *testing.T) {
	mc := newMockConnection()
	// Pre-create the blocked stream so the test can push deterministically;
	// the watcher reads this same channel via NotifyBlocked().
	mc.BlockedChan = make(chan connBlockState, 4)

	rec := &recordingExporter{}
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	sess.metrics = rec

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	// Baseline: connected and not degraded.
	if sl := sess.Health(ctx).ServiceLevel; sl == ports.ServiceLevelDegraded {
		t.Fatalf("session should not start degraded, got %v", sl)
	}

	// Broker engages flow control (resource alarm).
	mc.BlockedChan <- connBlockState{Active: true, Reason: "low on memory"}
	wait.Until(t, 2*time.Second, "session degraded while blocked", func() bool {
		return sess.Health(ctx).ServiceLevel == ports.ServiceLevelDegraded
	})

	h := sess.Health(ctx)
	if !h.Connected {
		t.Error("session should remain Connected while blocked")
	}
	if !h.Ready {
		t.Error("session should remain Ready while blocked (publishes stall, route stays up)")
	}
	var be *shared.BridgeError
	if !errors.As(h.LastError, &be) || be.Code != shared.ErrCodeBrokerBusy {
		t.Errorf("LastError = %v, want ErrBrokerBusy", h.LastError)
	}
	// The counter is emitted just after the blocked state is set, so poll
	// for it rather than reading once (the Health degrade we waited on
	// only proves setBlocked ran, not that Counter has yet).
	wait.Until(t, 2*time.Second, "blocked metric emitted", func() bool {
		return rec.count(MetricAMQP091Blocked) >= 1
	})

	// Broker clears flow control.
	mc.BlockedChan <- connBlockState{Active: false}
	wait.Until(t, 2*time.Second, "session recovered after unblock", func() bool {
		return sess.Health(ctx).ServiceLevel != ports.ServiceLevelDegraded
	})
	if lastErr := sess.Health(ctx).LastError; lastErr != nil {
		t.Errorf("LastError should clear after unblock, got %v", lastErr)
	}
}

// TestSession_SetBlocked_StaleWatcherAfterReconnect_DoesNotDegrade proves
// the generation guard on setBlocked (AMQP091). A blocked-watcher bound
// to a dropped connection can still deliver a buffered {Active:true} AFTER
// the session has reconnected onto a fresh, healthy connection. Without the
// guard that stale write pins the healthy connection to Degraded +
// ErrBrokerBusy — a broker resource alarm that no longer applies. The
// current connection identity is the generation token, so a write bound to
// a superseded connection must be ignored.
func TestSession_SetBlocked_StaleWatcherAfterReconnect_DoesNotDegrade(t *testing.T) {
	// c1 is the original connection; dropping it forces a reconnect onto c2.
	c1 := newMockConnection()
	c1.NotifyCloseChan = make(chan error, 1)
	c2 := newMockConnection()

	var dialCount atomic.Int32
	sess := newResilienceSession(func(string) (amqpConnection, error) {
		if dialCount.Add(1) == 1 {
			return c1, nil
		}
		return c2, nil
	})

	ctx := context.Background()
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	// Sanity: c1 is the current connection right after Start.
	sess.mu.Lock()
	started := sess.conn == c1
	sess.mu.Unlock()
	if !started {
		t.Fatal("expected c1 to be the current connection after Start")
	}

	// Drop c1 to force a reconnect; wait until c2 is the current connection.
	close(c1.NotifyCloseChan)
	wait.Until(t, 2*time.Second, "session reconnected onto c2", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.connected && sess.conn == c2
	})

	// The freshly reconnected connection is healthy.
	if sl := sess.Health(ctx).ServiceLevel; sl == ports.ServiceLevelDegraded {
		t.Fatalf("freshly reconnected session should not be degraded, got %v", sl)
	}

	// The stale watcher for the dropped c1 delivers its buffered alarm.
	// The generation guard must reject it, leaving c2 healthy.
	if sess.setBlocked(c1, true, "low on memory") {
		t.Fatal("setBlocked from the dropped c1 should be rejected (stale generation)")
	}
	h := sess.Health(ctx)
	if h.ServiceLevel == ports.ServiceLevelDegraded {
		t.Fatal("stale watcher write from dropped c1 degraded the healthy c2 (generation guard missing)")
	}
	if h.LastError != nil {
		t.Fatalf("stale watcher write set LastError on healthy c2: %v", h.LastError)
	}

	// Positive control: a write from the CURRENT connection is honoured,
	// proving the guard discriminates by connection rather than rejecting
	// every write.
	if !sess.setBlocked(c2, true, "low on memory") {
		t.Fatal("setBlocked from the current c2 should be honoured")
	}
	if sl := sess.Health(ctx).ServiceLevel; sl != ports.ServiceLevelDegraded {
		t.Fatalf("current-connection block should degrade session, got %v", sl)
	}
}
