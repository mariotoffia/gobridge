// Validates that the Receiver's waitAndReconnect handles a race where
// the session has ALREADY reconnected (SessionConnected was published)
// before the receiver gets to Subscribe. Also pins the contract that
// the 1s blind sleep has been replaced with event-driven reconnect.
package amqp10

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiver_WaitAndReconnect_AlreadyConnected_ProceedsImmediately
// validates that when the session is already connected, waitAndReconnect
// returns immediately rather than sleeping for 1 second.
func TestReceiver_WaitAndReconnect_AlreadyConnected_ProceedsImmediately(t *testing.T) {
	sess := newTestSession()
	sess.dial = mockDialFunc(&mockConn{}, nil)

	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	rerr := r.waitAndReconnect(ctx)
	elapsed := time.Since(start)

	if rerr != nil && rerr != context.DeadlineExceeded {
		t.Logf("waitAndReconnect returned %v (link creation may fail in unit test; that's OK)", rerr)
	}

	if elapsed > 500*time.Millisecond {
		t.Fatalf("waitAndReconnect took %v; it must proceed immediately when session "+
			"is already connected (the old 1s blind sleep is gone)", elapsed)
	}
}

// TestReceiver_WaitAndReconnect_FastReconnect_DoesNotMissEvent validates
// the race where SessionConnected is pushed BEFORE the receiver reaches
// waitAndReconnect. The receiver must still observe the connected state
// and proceed.
func TestReceiver_WaitAndReconnect_FastReconnect_DoesNotMissEvent(t *testing.T) {
	sess := newTestSession()
	sess.dial = mockDialFunc(&mockConn{}, nil)

	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// Simulate: SessionConnected was pushed BEFORE waitAndReconnect
	// even runs (e.g., reconnect happened while the receiver was still
	// processing the link error). We push it now with no subscribers.
	sess.pushEvent(ports.SessionConnected, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.waitAndReconnect(ctx) }()

	select {
	case err := <-done:
		if err != nil && err != context.DeadlineExceeded {
			t.Logf("waitAndReconnect returned %v (acceptable for unit test without real link)", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("waitAndReconnect hung — the SessionConnected pushed before Subscribe " +
			"was lost and there is no current-state probe to recover")
	}
}

// TestReceiver_WaitAndReconnect_WaitsForEvent validates that when the
// session is NOT connected, waitAndReconnect blocks until a
// SessionConnected event arrives.
func TestReceiver_WaitAndReconnect_WaitsForEvent(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		domain.SessionEphemeral, slog.Default())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.waitAndReconnect(ctx) }()

	time.Sleep(100 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("waitAndReconnect returned before SessionConnected event")
	default:
	}

	sess.pushEvent(ports.SessionConnected, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Logf("waitAndReconnect returned %v (acceptable — link creation fails without real broker)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitAndReconnect did not unblock after SessionConnected event")
	}

	_ = sess.Close(context.Background())
}

// TestReceiver_WaitAndReconnect_ContextCancel validates that
// waitAndReconnect returns ctx.Err() when the context is cancelled.
func TestReceiver_WaitAndReconnect_ContextCancel(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		domain.SessionEphemeral, slog.Default())

	r, err := NewReceiver(ReceiverConfig{Address: "queue/x"}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rerr := r.waitAndReconnect(ctx)
	if rerr != context.Canceled {
		t.Fatalf("waitAndReconnect = %v, want context.Canceled", rerr)
	}

	_ = sess.Close(context.Background())
}

// TestReceiver_WaitAndReconnect_NilSession validates that
// waitAndReconnect returns an error when the receiver has no session.
func TestReceiver_WaitAndReconnect_NilSession(t *testing.T) {
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/x"},
		session: nil,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	err := r.waitAndReconnect(context.Background())
	if err == nil {
		t.Fatal("expected error from waitAndReconnect with nil session")
	}
}
