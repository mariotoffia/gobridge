// ═══════════════════════════════════════════════
// Sender Confirm & Timeout Tests
//
// Validates applyTimeout behaviour and Close idempotency.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"testing"
	"time"
)

// TestSender_ApplyTimeout_NoDeadline validates that a timeout is applied
// when the context has no existing deadline.
func TestSender_ApplyTimeout_NoDeadline(t *testing.T) {
	s := NewSender(SenderConfig{Timeout: 5 * time.Second})

	ctx, cancel := s.applyTimeout(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline after applyTimeout on a context without deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Fatalf("remaining = %v, expected ~5s", remaining)
	}
}

// TestSender_ApplyTimeout_WithDeadline validates that an existing deadline
// is preserved.
func TestSender_ApplyTimeout_WithDeadline(t *testing.T) {
	s := NewSender(SenderConfig{Timeout: 5 * time.Second})

	existing := time.Now().Add(2 * time.Second)
	parentCtx, parentCancel := context.WithDeadline(context.Background(), existing)
	defer parentCancel()

	ctx, cancel := s.applyTimeout(parentCtx)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be preserved")
	}

	if !deadline.Equal(existing) {
		t.Fatalf("deadline = %v, want %v (parent deadline)", deadline, existing)
	}
}

// TestSender_Close_Idempotent validates that Close can be called multiple
// times without panic.
func TestSender_Close_Idempotent(t *testing.T) {
	s := NewSender(SenderConfig{})

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
