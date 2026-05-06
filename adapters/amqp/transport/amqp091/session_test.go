package amqp091

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

func testSession(opts ...func(*SessionOptions)) *Session {
	o := SessionOptions{
		BrokerURL:      "amqp://localhost:5672/",
		ConnectTimeout: 2 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return NewSession(o, domain.SessionEphemeral, slog.Default())
}

func connectSession(t *testing.T, s *Session) *mockConnection {
	t.Helper()
	mc := newMockConnection()
	s.dial = func(string) (amqpConnection, error) { return mc, nil }
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return mc
}

// drainEvents reads all pending events within a short window.
func drainEvents(ch <-chan ports.SessionEvent, timeout time.Duration) []ports.SessionEvent {
	var out []ports.SessionEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

// --- Start ---

// verifies Start returns an error when the session is already closed.
func TestSession_Start_Closed(t *testing.T) {
	s := testSession()
	_ = s.Close(context.Background())

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from Start on closed session")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, shared.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got code %s", be.Code)
	}
}

// verifies Start is idempotent — a second call is a no-op.
func TestSession_Start_Idempotent(t *testing.T) {
	s := testSession()
	mc := connectSession(t, s)
	defer func() { _ = s.Close(context.Background()) }()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	mc.mu.Lock()
	calls := mc.ChannelCalls
	mc.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected no extra Channel calls, got %d", calls)
	}
}

// verifies Start propagates dial errors.
func TestSession_Start_DialError(t *testing.T) {
	s := testSession()
	s.dial = func(string) (amqpConnection, error) {
		return nil, errors.New("connection refused")
	}

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from failed dial")
	}
}

// --- Close ---

// verifies Close is idempotent — multiple calls do not panic.
func TestSession_Close_Idempotent(t *testing.T) {
	s := testSession()
	connectSession(t, s)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// verifies Close calls Close on the underlying connection.
func TestSession_Close_ClosesConnection(t *testing.T) {
	s := testSession()
	mc := connectSession(t, s)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mc.mu.Lock()
	calls := mc.CloseCalls
	mc.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 Close call on connection, got %d", calls)
	}
}

// verifies Close closes the events channel.
func TestSession_Close_ClosesEventsChannel(t *testing.T) {
	s := testSession()
	connectSession(t, s)
	ch := s.Events()

	_ = s.Close(context.Background())

	// Channel should be drained and closed.
	timer := time.After(500 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timer:
			t.Fatal("events channel not closed within timeout")
		}
	}
}

// --- Health ---

// verifies Health returns ServiceLevelNone when not connected.
func TestSession_Health_NotConnected(t *testing.T) {
	s := testSession()

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelNone)
	}
	if h.Connected {
		t.Error("Connected should be false")
	}
}

// verifies Health returns ServiceLevelFull when connected with no plan.
func TestSession_Health_ConnectedNoPlan(t *testing.T) {
	s := testSession()
	s.mu.Lock()
	s.connected = true
	s.conn = newMockConnection()
	s.mu.Unlock()

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Errorf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}
}

// verifies Health returns ServiceLevelFull when all subscriptions are active.
func TestSession_Health_FullSubscriptions(t *testing.T) {
	s := testSession()
	s.mu.Lock()
	s.connected = true
	s.conn = newMockConnection()
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "q1"},
			{Topic: "q2"},
		},
	}
	s.activeSubs = map[string]bool{"q1": true, "q2": true}
	s.mu.Unlock()

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelFull {
		t.Errorf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelFull)
	}
	if h.SubscriptionsWanted != 2 {
		t.Errorf("SubscriptionsWanted = %d, want 2", h.SubscriptionsWanted)
	}
	if h.SubscriptionsActive != 2 {
		t.Errorf("SubscriptionsActive = %d, want 2", h.SubscriptionsActive)
	}
}

// verifies Health returns ServiceLevelDegraded when only some subscriptions are active.
func TestSession_Health_Degraded(t *testing.T) {
	s := testSession()
	s.mu.Lock()
	s.connected = true
	s.conn = newMockConnection()
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "q1"},
			{Topic: "q2"},
			{Topic: "q3"},
		},
	}
	s.activeSubs = map[string]bool{"q1": true}
	s.mu.Unlock()

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelDegraded {
		t.Errorf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelDegraded)
	}
}

// verifies Health returns ServiceLevelNone when connected but zero active subscriptions with a plan.
func TestSession_Health_ConnectedZeroActiveSubs(t *testing.T) {
	s := testSession()
	s.mu.Lock()
	s.connected = true
	s.conn = newMockConnection()
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "q1"}},
	}
	s.activeSubs = map[string]bool{}
	s.mu.Unlock()

	h := s.Health(context.Background())
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel = %q, want %q", h.ServiceLevel, ports.ServiceLevelNone)
	}
}

// --- Events ---

// verifies Events returns the session event channel.
func TestSession_Events(t *testing.T) {
	s := testSession()
	defer func() { _ = s.Close(context.Background()) }()
	ch := s.Events()
	if ch == nil {
		t.Fatal("Events() returned nil")
	}
}

// --- pushEvent ---

// verifies pushEvent evicts the oldest event when the channel is full.
func TestSession_PushEvent_EvictsOldest(t *testing.T) {
	s := testSession()
	defer func() { _ = s.Close(context.Background()) }()
	// Fill the 16-slot event buffer.
	for i := 0; i < 16; i++ {
		s.pushEvent(ports.SessionReconnecting, nil)
	}

	// Push one more — should evict the oldest and insert the new one.
	s.pushEvent(ports.SessionConnected, nil)

	events := drainEvents(s.Events(), 200*time.Millisecond)
	if len(events) != 16 {
		t.Fatalf("expected 16 events after eviction, got %d", len(events))
	}
	if events[len(events)-1].Type != ports.SessionConnected {
		t.Errorf("last event type = %d, want SessionConnected (%d)",
			events[len(events)-1].Type, ports.SessionConnected)
	}
}

// verifies pushEvent is a no-op on a closed session.
func TestSession_PushEvent_ClosedNoop(t *testing.T) {
	s := testSession()
	_ = s.Close(context.Background())

	// Should not panic.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.pushEvent(ports.SessionError, errors.New("boom"))
}

// --- Reconcile ---

// verifies Reconcile returns an error when the session has not been started.
func TestSession_Reconcile_NotStarted(t *testing.T) {
	s := testSession()

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "q1"}},
	}
	err := s.Reconcile(context.Background(), plan)
	if err == nil {
		t.Fatal("expected error from Reconcile on un-started session")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if !errors.Is(be, shared.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got code %s", be.Code)
	}
}

// --- brokerURL / safeBrokerURL ---

// verifies brokerURL injects credentials from session options.
func TestSession_BrokerURL(t *testing.T) {
	s := testSession(func(o *SessionOptions) {
		o.Username = "admin"
		o.Password = "s3cret"
	})

	got := s.brokerURL()
	want := "amqp://admin:s3cret@localhost:5672/"
	if got != want {
		t.Errorf("brokerURL() = %q, want %q", got, want)
	}
}

// verifies safeBrokerURL redacts credentials for safe logging.
func TestSession_SafeBrokerURL(t *testing.T) {
	s := testSession(func(o *SessionOptions) {
		o.Username = "admin"
		o.Password = "s3cret"
	})

	got := s.safeBrokerURL()
	want := "amqp://REDACTED@localhost:5672/"
	if got != want {
		t.Errorf("safeBrokerURL() = %q, want %q", got, want)
	}
}

// verifies safeBrokerURL returns an unaltered URL when no credentials are set.
func TestSession_SafeBrokerURL_NoCredentials(t *testing.T) {
	s := testSession()

	got := s.safeBrokerURL()
	want := "amqp://localhost:5672/"
	if got != want {
		t.Errorf("safeBrokerURL() = %q, want %q", got, want)
	}
}
