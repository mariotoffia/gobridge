package paho

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-SAC: Session.Start does not check s.closed before connecting.
//
// Defect:
//
//	After Close() the session has:
//	  - s.closed = true
//	  - s.events channel closed
//	  - s.cm = nil
//	If Start() is then called, the (cm == nil) guard at the top is true,
//	so Start proceeds to attempt a fresh connection. On success it sets
//	s.cm = newCM and s.connected = true — but s.events is permanently
//	closed and no event will ever be readable from it again. Operators
//	may believe the session is healthy when in reality the event channel
//	is dead.
//
//	pushEvent() correctly drops events when s.closed is true, but Start
//	itself happily revives the cm/connected fields, leading to the
//	"zombie session" state.
//
// Fix:
//
//	Start() must check s.closed under the same lock as the cm==nil guard
//	and return a typed ErrUnavailable when the session has already been
//	closed. The session is single-use; once Close has been called it
//	cannot be reused.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugSAC_StartAfterClose_ReturnsErrorAndDoesNotConnect verifies the
// fix: Start invoked on a closed Session must return an error WITHOUT
// attempting any connection.
func TestBugSAC_StartAfterClose_ReturnsErrorAndDoesNotConnect(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737 unreachable
		ClientID:       "bug-sac-1",
		ConnectTimeout: 200 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("Start after Close panicked: %v", rv)
		}
	}()

	// Use a generous deadline; if Start were buggy it would block
	// trying to connect to the unreachable broker for 200ms+. The fix
	// makes it return immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	err := s.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BUG-SAC: Start after Close must return an error " +
			"(closed sessions are single-use)")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("BUG-SAC: Start after Close should return *domain.BridgeError, got %T: %v", err, err)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Errorf("BUG-SAC: err code = %s, want %s", be.Code, domain.ErrUnavailable.Code)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("BUG-SAC: Start took %v after Close — must return immediately, not attempt connect", elapsed)
	}

	// Invariant: cm must remain nil after a Start-on-closed-session
	// call (no connection attempted, no zombie cm installed).
	s.mu.Lock()
	cm := s.cm
	s.mu.Unlock()
	if cm != nil {
		t.Fatal("BUG-SAC: cm must remain nil after Start-on-closed; otherwise events channel is dead but session looks alive")
	}
}

// TestBugSAC_HealthAfterStartAfterClose_StillReportsDisconnected
// verifies that even if a buggy implementation managed to set s.cm,
// the closed events channel makes the session unusable. The fix
// guarantees Health continues to report the session as not connected.
func TestBugSAC_HealthAfterStartAfterClose_StillReportsDisconnected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "bug-sac-2",
		ConnectTimeout: 200 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	_ = s.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = s.Start(ctx)

	h := s.Health(context.Background())
	if h.Connected {
		t.Fatal("BUG-SAC: Health.Connected must be false after Start-on-closed")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Fatalf("BUG-SAC: Health.ServiceLevel = %v, want None", h.ServiceLevel)
	}
}

// TestBugSAC_StartAfterClose_DoesNotLeakConnectionManager verifies that
// no autopaho ConnectionManager is created (and therefore no goroutine
// leak) when Start is called on a closed session. Without the fix, a
// zombie CM (and its reconnect loop goroutines) would be started.
func TestBugSAC_StartAfterClose_DoesNotLeakConnectionManager(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "bug-sac-3",
		ConnectTimeout: 200 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	_ = s.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = s.Start(ctx)

	if got := s.ConnectionManager(); got != nil {
		t.Fatal("BUG-SAC: ConnectionManager() must remain nil after Start-on-closed")
	}
}

// TestBugSAC_ReconcileAfterStartAfterClose_ReturnsError verifies the
// downstream consequence: even if the buggy Start succeeded, Reconcile
// should still report the session as unavailable. The fix ensures
// Reconcile sees s.cm == nil.
func TestBugSAC_ReconcileAfterStartAfterClose_ReturnsError(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "bug-sac-4",
		ConnectTimeout: 200 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	_ = s.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = s.Start(ctx)

	err := s.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "t/x", QoS: 1}},
	})
	if err == nil {
		t.Fatal("BUG-SAC: Reconcile after Start-on-closed must error")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("BUG-SAC: err type = %T, want *domain.BridgeError", err)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Errorf("BUG-SAC: err code = %s, want %s", be.Code, domain.ErrUnavailable.Code)
	}
}
