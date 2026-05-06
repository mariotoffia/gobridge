package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func newTestSession() *Session {
	opts := SessionOptions{
		Address:        "amqp://localhost:5672",
		ConnectTimeout: 2 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	return NewSession(opts, domain.SessionPersistent, slog.Default())
}

func TestSession_Start_ClosedSession(t *testing.T) {
	// verifies that Start returns an error when the session has been closed
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("Start() on closed session should return error")
	}

	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T: %v", err, err)
	}
}

func TestSession_Start_Idempotent(t *testing.T) {
	// verifies that calling Start twice is a no-op on the second call
	s := newTestSession()
	mc := &mockConn{}
	s.dial = mockDialFunc(mc, nil)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("second Start() should be no-op, got error: %v", err)
	}
}

func TestSession_Close_Idempotent(t *testing.T) {
	// verifies that calling Close multiple times is safe
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("first Close() error: %v", err)
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("second Close() should be no-op, got error: %v", err)
	}
}

func TestSession_Close_ClosesEventsChannel(t *testing.T) {
	// verifies that the events channel is closed after Close
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	ch := s.Events()
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Drain any pending events, then verify the channel is closed.
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed as expected
			}
		case <-timer.C:
			t.Fatal("events channel was not closed after Close()")
		}
	}
}

func TestSession_Health_NotConnected(t *testing.T) {
	// verifies ServiceLevelNone when the session is not connected
	s := newTestSession()

	h := s.Health(context.Background())
	if h.Connected {
		t.Fatal("expected Connected=false for new session")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Fatalf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelNone)
	}
}

func TestSession_Health_Connected_NoPlan(t *testing.T) {
	// verifies ServiceLevelFull when connected with no reconciled plan
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	h := s.Health(context.Background())
	if !h.Connected {
		t.Fatal("expected Connected=true after Start")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}
}

func TestSession_Health_Connected_WithPlan(t *testing.T) {
	// verifies ServiceLevelFull when connected with a reconciled plan
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "test/topic"}},
	}
	if err := s.Reconcile(context.Background(), plan); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}

	h := s.Health(context.Background())
	if !h.Connected {
		t.Fatal("expected Connected=true")
	}
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}
	if h.SubscriptionsWanted != 1 {
		t.Fatalf("SubscriptionsWanted = %d, want 1", h.SubscriptionsWanted)
	}
}

func TestSession_Events_Returns_Channel(t *testing.T) {
	// verifies Events() returns a non-nil channel
	s := newTestSession()
	defer func() { _ = s.Close(context.Background()) }()
	ch := s.Events()
	if ch == nil {
		t.Fatal("Events() returned nil channel")
	}
}

func TestSession_Reconcile_NotStarted(t *testing.T) {
	// verifies Reconcile returns ErrUnavailable when session is not started
	s := newTestSession()

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "test/topic"}},
	}
	err := s.Reconcile(context.Background(), plan)
	if err == nil {
		t.Fatal("Reconcile() on unstarted session should return error")
	}

	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrCodeUnavailable {
		t.Fatalf("error code = %q, want %q", be.Code, shared.ErrCodeUnavailable)
	}
}

func TestSession_PushEvent_Overflow(t *testing.T) {
	// verifies that pushEvent to a full channel evicts the oldest event
	s := newTestSession()
	defer func() { _ = s.Close(context.Background()) }()
	// Fill the events channel to capacity.
	for i := 0; i < eventChannelSize; i++ {
		s.pushEvent(ports.SessionReconnecting, nil)
	}

	// Push one more — should succeed by evicting the oldest.
	s.pushEvent(ports.SessionConnected, nil)

	// Drain and verify we got exactly eventChannelSize events.
	count := 0
	for {
		select {
		case <-s.events:
			count++
		default:
			if count != eventChannelSize {
				t.Fatalf("expected %d events after overflow push, got %d", eventChannelSize, count)
			}
			return
		}
	}
}

func TestSession_NotifyDisconnect(t *testing.T) {
	// verifies notifyDisconnect clears connection state and signals reconnectCh
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
		t.Fatal("expected conn to be nil after notifyDisconnect")
	}
	if connected {
		t.Fatal("expected connected=false after notifyDisconnect")
	}

	// Verify reconnectCh received a signal.
	select {
	case <-s.reconnectCh:
	default:
		t.Fatal("reconnectCh should have a signal after notifyDisconnect")
	}

	// Verify a SessionDisconnected event was pushed.
	select {
	case ev := <-s.events:
		if ev.Type != ports.SessionDisconnected {
			t.Fatalf("event type = %v, want SessionDisconnected", ev.Type)
		}
	default:
		t.Fatal("expected SessionDisconnected event")
	}
}

func TestSession_NotifyDisconnect_AlreadyClosed(t *testing.T) {
	// verifies notifyDisconnect is a no-op on a closed session
	s := newTestSession()
	mc := &mockConn{}
	s.mu.Lock()
	s.closed = true
	s.conn = mc
	s.connected = true
	s.mu.Unlock()

	s.notifyDisconnect(mc, errors.New("link detached"))

	s.mu.Lock()
	conn := s.conn
	connected := s.connected
	s.mu.Unlock()

	if conn == nil {
		t.Fatal("conn should remain set when session is closed")
	}
	if !connected {
		t.Fatal("connected should remain true when session is closed")
	}

	select {
	case <-s.reconnectCh:
		t.Fatal("reconnectCh should not have a signal when closed")
	default:
	}
}

func TestSession_RedactURL(t *testing.T) {
	// verifies redactURL masks credentials and handles edge cases
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no credentials",
			input: "amqp://localhost:5672",
			want:  "amqp://localhost:5672",
		},
		{
			name:  "with credentials",
			input: "amqp://user:secret@broker.example.com:5672",
			want:  "amqp://%2A%2A%2A@broker.example.com:5672",
		},
		{
			name:  "invalid URL",
			input: "://bad",
			want:  "<invalid-url>",
		},
		{
			name:  "username only",
			input: "amqp://admin@localhost:5672/vhost",
			want:  "amqp://%2A%2A%2A@localhost:5672/vhost",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURL(tc.input)
			if got != tc.want {
				t.Fatalf("redactURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
