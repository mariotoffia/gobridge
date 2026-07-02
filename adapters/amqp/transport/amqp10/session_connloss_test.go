// Validates connection-loss handling driven by Conn.Done() (finding 1)
// and receiver-link-aware Health reporting (finding 4).
package amqp10

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// waitForEventType drains ch until an event of type want arrives,
// ignoring intermediate lifecycle events. The deadline is a failure
// guard only — it returns as soon as the target event is observed, so
// the test contains no fixed sleeps.
func waitForEventType(t *testing.T, ch <-chan ports.SessionEvent, want ports.SessionEventType, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event channel closed while waiting for event type %v", want)
			}
			if ev.Type == want {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for session event type %v", want)
		}
	}
}

// TestSession_Monitor_ConnDone_TriggersReconnect proves the finding-1
// fix: when the underlying connection's Done() channel fires, the monitor
// clears the dead connection and reconnects instead of busy-spinning on
// the closed channel while still reporting Connected=true.
func TestSession_Monitor_ConnDone_TriggersReconnect(t *testing.T) {
	s := newTestSession()

	conn1 := &mockConn{}
	conn2 := &mockConn{}
	var calls atomic.Int32
	s.dial = func(_ context.Context, _ SessionOptions, _ amqp10Credentials) (amqpConn, error) {
		if calls.Add(1) == 1 {
			return conn1, nil
		}
		return conn2, nil
	}

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	sub, unsub := s.Subscribe()
	defer unsub()

	// Signal connection loss on conn1's Done() channel.
	conn1.triggerDone()

	waitForEventType(t, sub, ports.SessionDisconnected, 2*time.Second)
	waitForEventType(t, sub, ports.SessionConnected, 2*time.Second)

	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	s.mu.Unlock()
	if conn != conn2 {
		t.Fatalf("expected reconnect to conn2, got %v", conn)
	}
	if !connected {
		t.Fatal("expected connected=true after reconnect")
	}

	conn1.mu.Lock()
	conn1Closed := conn1.closed
	conn1.mu.Unlock()
	if !conn1Closed {
		t.Fatal("expected the lost connection (conn1) to be closed by handleConnLost")
	}
}

// TestSession_Monitor_ConnDone_NoSpinWhenStale validates the stale guard:
// a Done() wakeup for a connection the session no longer holds is a no-op
// (it must not tear down the current connection).
func TestSession_Monitor_ConnDone_NoSpinWhenStale(t *testing.T) {
	s := newTestSession()
	current := &mockConn{}
	s.mu.Lock()
	s.conn = current
	s.connected = true
	s.mu.Unlock()

	stale := &mockConn{}
	s.handleConnLost(context.Background(), stale)

	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	s.mu.Unlock()
	if conn != current || !connected {
		t.Fatal("stale Done() wakeup must not disturb the current connection")
	}
}

// TestSession_Health_ReceiverLinkDown_Degrades proves the finding-4 fix:
// Health reports Degraded (not Full) when a registered receiver's link is
// down while the session connection is still alive.
func TestSession_Health_ReceiverLinkDown_Degrades(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.mu.Unlock()

	r := &Receiver{} // pointer used only as a health-tracking map key

	// Sanity: with no registered receivers Health stays Full (preserves
	// the pre-existing connected-with-no-receivers behaviour).
	if h := s.Health(context.Background()); h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("no receivers: ServiceLevel = %q, want full", h.ServiceLevel)
	}

	s.registerReceiver(r)

	// Link up -> Full, 1 wanted / 1 active.
	s.markReceiverLink(r, true)
	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("link up: ServiceLevel = %q, want full", h.ServiceLevel)
	}
	if h.SubscriptionsWanted != 1 || h.SubscriptionsActive != 1 {
		t.Fatalf("link up: wanted/active = %d/%d, want 1/1", h.SubscriptionsWanted, h.SubscriptionsActive)
	}

	// Link down while connected -> Degraded, active drops.
	s.markReceiverLink(r, false)
	h = s.Health(context.Background())
	if !h.Connected {
		t.Fatal("expected Connected=true (only the link is down)")
	}
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("link down: ServiceLevel = %q, want degraded", h.ServiceLevel)
	}
	if h.SubscriptionsWanted != 1 || h.SubscriptionsActive != 0 {
		t.Fatalf("link down: wanted/active = %d/%d, want 1/0", h.SubscriptionsWanted, h.SubscriptionsActive)
	}

	// The bulk helper used on connection loss flips every link down.
	s.markReceiverLink(r, true)
	s.mu.Lock()
	s.markAllReceiversDownLocked()
	s.mu.Unlock()
	if h := s.Health(context.Background()); h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Fatalf("after markAllReceiversDownLocked: ServiceLevel = %q, want degraded", h.ServiceLevel)
	}

	// Unregister -> no tracked receivers -> back to Full.
	s.unregisterReceiver(r)
	if h := s.Health(context.Background()); h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("after unregister: ServiceLevel = %q, want full", h.ServiceLevel)
	}
}
