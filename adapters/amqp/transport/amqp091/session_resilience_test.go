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
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
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
		opts:           opts,
		mode:           connectivity.SessionMode("consumer"),
		logger:         slog.Default(),
		metrics:        &ports.NoopExporter{},
		dial:           dial,
		events:         make(chan ports.SessionEvent, 16),
		activeSubs:     make(map[string]bool),
		reconnected:    make(chan struct{}, 1),
		forceReconnect: make(chan struct{}, 1),
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
	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// TestSession_Start_IdempotentSecondCall validates that calling Start twice
// is a no-op and dial is only called once.
func TestSession_Start_IdempotentSecondCall(t *testing.T) {
	dialCount := 0
	mc := newMockConnection()

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

// TestSession_Close_HonoursContext_HalfDeadBroker is the M-2 regression:
// Session.Close must race conn.Close() against ctx so a half-dead broker
// (whose Connection.Close-Ok never arrives) cannot wedge shutdown for the
// SDK's ~20-30s handshake timeout.
//
// Counterfactual (call conn.Close() directly): Close blocks on the stalled
// CloseFn forever and never returns, so RequireReceive times out.
func TestSession_Close_HonoursContext_HalfDeadBroker(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the detached conn.Close() finish after the test

	mc := newMockConnection()
	mc.CloseFn = func() error {
		<-release // broker never answers Connection.Close-Ok
		return nil
	}
	s := newResilienceSession(func(url string) (amqpConnection, error) {
		return mc, nil
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's deadline already blown when Close begins

	done := make(chan error, 1)
	go func() { done <- s.Close(ctx) }()

	err := wait.RequireReceive(t, done, 2*time.Second)
	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeUnavailable {
		t.Fatalf("Close on cancelled ctx: got %v, want ErrUnavailable (mapped context.Canceled)", err)
	}
}

// TestSession_Close_HonoursContext_StuckReconnectGoroutine is the FIX 3
// regression (a follow-up to M-2): Close's wait for the background reconnect
// goroutine to exit must be ctx-bounded. That goroutine can be parked in an
// SDK topology call (dial/declare) that ignores ctx, so an unconditional
// <-bgDone wait would overrun the caller's deadline by up to a heartbeat.
// Close must return on ctx and let the goroutine finish detached.
//
// The stuck goroutine is simulated deterministically: it closes bgDone only
// when released, and cancel() does NOT unblock it (mirroring the amqp091-go
// client's non-context-aware topology calls).
//
// Counterfactual (revert to an unconditional <-done): Close blocks until the
// goroutine is released at test cleanup and wait.RequireReceive times out.
func TestSession_Close_HonoursContext_StuckReconnectGoroutine(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the "stuck" goroutine finish after the test

	s := newResilienceSession(nil)

	bgDone := make(chan struct{})
	go func() {
		<-release // parked in a ctx-ignoring SDK call
		close(bgDone)
	}()
	s.mu.Lock()
	s.bgDone = bgDone
	s.cancel = func() {} // cancel does not unblock the ctx-ignoring call
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's deadline already blown when Close begins

	done := make(chan error, 1)
	go func() { done <- s.Close(ctx) }()

	// With FIX 3 Close returns promptly on ctx (no connection to close, so
	// nil); without it, it blocks on <-bgDone until release and this times
	// out.
	if err := wait.RequireReceive(t, done, 2*time.Second); err != nil {
		t.Fatalf("Close should return nil (no connection to close), got %v", err)
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
	mc := newMockConnection()

	s := newResilienceSession(func(url string) (amqpConnection, error) {
		time.Sleep(200 * time.Millisecond) // OTHER: simulates slow dial to test timeout + leaked connection cleanup
		return mc, nil
	})
	s.opts.ConnectTimeout = 10 * time.Millisecond

	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}

	wait.Until(t, 2*time.Second, "leaked connection closed", mc.IsClosed)
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
// TestSession_Reconcile_BeforeStart_RetainsPlanNoError verifies the MINOR
// remediation: Reconcile before Start is a valid ordering — the plan is
// retained (Start applies it) so it returns nil, not an error.
func TestSession_Reconcile_BeforeStart_RetainsPlanNoError(t *testing.T) {
	s := newResilienceSession(nil)

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "q1"}},
	}
	if err := s.Reconcile(context.Background(), plan); err != nil {
		t.Fatalf("Reconcile before Start must not error (plan is retained): %v", err)
	}
	s.mu.Lock()
	retained := s.plan
	s.mu.Unlock()
	if retained == nil || len(retained.Subscriptions) != 1 || retained.Subscriptions[0].Topic != "q1" {
		t.Fatalf("Reconcile before Start must retain the plan for Start to apply, got %+v", retained)
	}
}

// TestSession_Reconcile_Closed_ReturnsUnavailable verifies a Reconcile on an
// already-closed session still fails — the plan can never be applied.
func TestSession_Reconcile_Closed_ReturnsUnavailable(t *testing.T) {
	s := newResilienceSession(nil)
	s.closed = true

	err := s.Reconcile(context.Background(), connectivity.SessionPlan{})
	if err == nil {
		t.Fatal("expected error from Reconcile on a closed session")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeUnavailable {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
