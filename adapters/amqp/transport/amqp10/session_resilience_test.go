// ═══════════════════════════════════════════════
// Session Resilience Tests
//
// Validates notifyDisconnect connection identity guard,
// connect() old-connection cleanup, close safety, and
// Health() reporting accuracy.
// ═══════════════════════════════════════════════
package amqp10

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSession_NotifyDisconnect_StaleConnection validates that
// notifyDisconnect ignores stale connection pointers, preventing
// destruction of a freshly reconnected connection.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	oldConn ──error──▶ notifyDisconnect(oldConn)
//	        ↓ (meanwhile session reconnected to newConn)
//	        → stale notification ignored
//
// ───────────────────────────────────────────────
func TestSession_NotifyDisconnect_StaleConnection(t *testing.T) {
	s := newTestSession()
	oldConn := &mockConn{}
	newConn := &mockConn{}

	s.mu.Lock()
	s.conn = newConn
	s.connected = true
	s.mu.Unlock()

	s.notifyDisconnect(oldConn, errors.New("old link detached"))

	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	s.mu.Unlock()

	if conn != newConn {
		t.Fatal("expected new connection to be preserved")
	}
	if !connected {
		t.Fatal("expected connected=true (stale disconnect should be no-op)")
	}

	select {
	case <-s.reconnectCh:
		t.Fatal("reconnectCh should not be signalled for stale connection")
	default:
	}
}

// TestSession_NotifyDisconnect_MatchingConnection validates that
// notifyDisconnect clears state when the correct connection is reported.
func TestSession_NotifyDisconnect_MatchingConnection(t *testing.T) {
	s := newTestSession()
	mc := &mockConn{}

	s.mu.Lock()
	s.conn = mc
	s.connected = true
	s.mu.Unlock()

	s.notifyDisconnect(mc, errors.New("link detached"))

	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	s.mu.Unlock()

	if conn != nil {
		t.Fatal("expected conn=nil after matching disconnect")
	}
	if connected {
		t.Fatal("expected connected=false after matching disconnect")
	}

	select {
	case ev := <-s.events:
		if ev.Type != ports.SessionDisconnected {
			t.Fatalf("event type = %v, want SessionDisconnected", ev.Type)
		}
	default:
		t.Fatal("expected SessionDisconnected event")
	}
}

// TestSession_Close_NoConnection validates that closing a session
// without any established connection is safe.
func TestSession_Close_NoConnection(t *testing.T) {
	s := newTestSession()

	err := s.Close(context.Background())
	if err != nil {
		t.Fatalf("unexpected Close error: %v", err)
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()

	if !closed {
		t.Fatal("expected session to be closed")
	}
}

// TestSession_Health_Connected validates Health returns ServiceLevelFull
// when connected.
func TestSession_Health_Connected(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.conn = &mockConn{}
	s.connected = true
	s.mu.Unlock()

	h := s.Health(context.Background())
	if !h.Connected {
		t.Fatal("expected Connected=true")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("ServiceLevel = %s, want full", h.ServiceLevel)
	}
}

// TestSession_Health_Disconnected validates Health returns
// ServiceLevelNone when not connected.
func TestSession_Health_Disconnected(t *testing.T) {
	s := newTestSession()
	h := s.Health(context.Background())
	if h.Connected {
		t.Fatal("expected Connected=false")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Fatalf("ServiceLevel = %s, want none", h.ServiceLevel)
	}
}

// TestSession_Start_AlreadyClosed validates that Start on a closed
// session returns ErrUnavailable.
func TestSession_Start_AlreadyClosed(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from Start on closed session")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) || be.Code != domain.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// TestSession_PushEvent_FullChannel validates the drop-oldest eviction
// when the event channel is full.
func TestSession_PushEvent_FullChannel(t *testing.T) {
	s := newTestSession()

	for range eventChannelSize {
		s.pushEvent(ports.SessionReconnecting, nil)
	}

	s.pushEvent(ports.SessionConnected, nil)

	drained := 0
	for {
		select {
		case <-s.events:
			drained++
		default:
			goto done
		}
	}
done:
	if drained != eventChannelSize {
		t.Fatalf("expected %d events, got %d", eventChannelSize, drained)
	}
}

// TestSession_Reconcile_NotConnected validates Reconcile returns
// ErrUnavailable when session is not connected.
func TestSession_Reconcile_NotConnected(t *testing.T) {
	s := newTestSession()
	err := s.Reconcile(context.Background(), domain.SessionPlan{})
	if err == nil {
		t.Fatal("expected error from Reconcile when not connected")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) || be.Code != domain.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
