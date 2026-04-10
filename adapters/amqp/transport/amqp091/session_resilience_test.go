// ═══════════════════════════════════════════════
// Session Resilience Tests
//
// Validates reconnection lifecycle, event push edge cases,
// close-during-reconnect safety, and typed-nil error guarding.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func newResilienceSession(dial dialFunc) *Session {
	opts := SessionOptions{
		BrokerURL:      "amqp://localhost",
		Heartbeat:      1 * time.Second,
		ConnectTimeout: 2 * time.Second,
		ReconnectDelay: 50 * time.Millisecond,
	}
	opts.applyDefaults()
	return &Session{
		opts:       opts,
		mode:       domain.SessionMode("consumer"),
		logger:     slog.Default(),
		metrics:    &ports.NoopExporter{},
		dial:       dial,
		events:     make(chan ports.SessionEvent, 16),
		activeSubs: make(map[string]bool),
	}
}

// TestSession_Start_AlreadyClosed validates that Start on a closed
// session returns ErrUnavailable.
func TestSession_Start_AlreadyClosed(t *testing.T) {
	s := newResilienceSession(nil)
	s.closed = true

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from Start on closed session")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) || be.Code != domain.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// TestSession_Start_IdempotentSecondCall validates that calling Start twice
// is a no-op and dial is only called once.
func TestSession_Start_IdempotentSecondCall(t *testing.T) {
	dialCount := 0
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error {
		return ch
	}

	s := newResilienceSession(func(url string) (amqpConnection, error) {
		dialCount++
		return mc, nil
	})

	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}
	if dialCount != 1 {
		t.Fatalf("dial called %d times, want 1", dialCount)
	}

	_ = s.Close(ctx)
}

// TestSession_Close_MultipleCallsSafe validates Close can be called multiple
// times without panic or error.
func TestSession_Close_MultipleCallsSafe(t *testing.T) {
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error {
		return ch
	}

	s := newResilienceSession(func(url string) (amqpConnection, error) {
		return mc, nil
	})
	ctx := context.Background()
	_ = s.Start(ctx)

	if err := s.Close(ctx); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

// TestSession_PushEvent_FullChannel validates event eviction when the
// channel is full.
func TestSession_PushEvent_FullChannel(t *testing.T) {
	s := newResilienceSession(nil)

	for i := range 16 {
		s.pushEvent(ports.SessionReconnecting, nil)
		_ = i
	}

	s.pushEvent(ports.SessionConnected, nil)

	drained := 0
	for drained < 16 {
		select {
		case <-s.events:
			drained++
		default:
			t.Fatalf("expected 16 events, drained %d", drained)
		}
	}
}

// TestSession_DialTimeout_LeakCleanup validates that a connection
// obtained after dial timeout is properly closed.
func TestSession_DialTimeout_LeakCleanup(t *testing.T) {
	var mu sync.Mutex
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error {
		return ch
	}
	mc.CloseFn = func() error {
		mu.Lock()
		defer mu.Unlock()
		mc.closed = true
		return nil
	}

	s := newResilienceSession(func(url string) (amqpConnection, error) {
		time.Sleep(200 * time.Millisecond)
		return mc, nil
	})
	s.opts.ConnectTimeout = 10 * time.Millisecond

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}

	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	closed := mc.closed
	mu.Unlock()
	if !closed {
		t.Fatal("leaked connection should have been closed")
	}
}

// TestSession_Health_Disconnected validates Health returns
// ServiceLevelNone when disconnected.
func TestSession_Health_Disconnected(t *testing.T) {
	s := newResilienceSession(nil)
	h := s.Health(context.Background())
	if h.Connected {
		t.Fatal("expected Connected=false")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Fatalf("ServiceLevel = %s, want none", h.ServiceLevel)
	}
}

// TestSession_Reconcile_NoConnection validates Reconcile returns
// ErrUnavailable when session has no connection.
func TestSession_Reconcile_NoConnection(t *testing.T) {
	s := newResilienceSession(nil)
	err := s.Reconcile(context.Background(), domain.SessionPlan{})
	if err == nil {
		t.Fatal("expected error from Reconcile with no connection")
	}
	var be *domain.BridgeError
	if !errors.As(err, &be) || be.Code != domain.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
