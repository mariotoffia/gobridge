// ═══════════════════════════════════════════════
// Delivery Settlement Bug Tests
//
// Validates the sync.Once settlement idempotency,
// specifically testing BUG-2: when Ack fails, subsequent
// Retry should not silently return nil.
//
// Scenario (BUG-2):
// ───────────────────────────────────────────────
//
//	Ack() → AcceptMessage fails → returns error ✓
//	Retry() → once.Do is no-op → returns nil ✗ (should return error)
//
// ───────────────────────────────────────────────
// ═══════════════════════════════════════════════
package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestDelivery_AckFails_ThenRetry_ReportsError exposes BUG-2: when Ack
// fails, a subsequent Retry call should indicate the delivery was already
// settled (or attempted), not silently return nil.
func TestDelivery_AckFails_ThenRetry_ReportsError(t *testing.T) {
	settler := newMockSettler()
	settler.acceptErr = errors.New("network error during accept")

	env := &domain.Envelope{ID: "bug2-test"}
	msg := &amqp.Message{}
	d := NewDelivery(env, msg, settler, slog.Default(), &ports.NoopExporter{}, nil)

	err1 := d.Ack(context.Background())
	if err1 == nil {
		t.Fatal("Ack() should have returned an error")
	}

	err2 := d.Retry(context.Background(), 0, nil)
	// BUG-2: With current sync.Once implementation, err2 is nil even
	// though the delivery was never successfully settled. After the fix,
	// err2 should be non-nil (ErrAlreadySettled or the original error).
	if err2 == nil {
		t.Fatal("Retry() after failed Ack() should not silently succeed — " +
			"the delivery was never settled (BUG-2: sync.Once swallows failure)")
	}
}

// TestDelivery_RetryFails_ThenAck_ReportsError validates the reverse case.
func TestDelivery_RetryFails_ThenAck_ReportsError(t *testing.T) {
	settler := newMockSettler()
	settler.releaseErr = errors.New("network error during release")

	env := &domain.Envelope{ID: "bug2-reverse"}
	msg := &amqp.Message{}
	d := NewDelivery(env, msg, settler, slog.Default(), &ports.NoopExporter{}, nil)

	err1 := d.Retry(context.Background(), 0, nil)
	if err1 == nil {
		t.Fatal("Retry() should have returned an error")
	}

	err2 := d.Ack(context.Background())
	if err2 == nil {
		t.Fatal("Ack() after failed Retry() should not silently succeed (BUG-2)")
	}
}

// TestDelivery_ConcurrentSettlement validates that concurrent Ack/Retry
// calls from multiple goroutines are safe and only one settlement occurs.
func TestDelivery_ConcurrentSettlement(t *testing.T) {
	settler := newMockSettler()
	env := &domain.Envelope{ID: "concurrent-test"}
	msg := &amqp.Message{}
	d := NewDelivery(env, msg, settler, slog.Default(), &ports.NoopExporter{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				_ = d.Ack(context.Background())
			} else {
				_ = d.Retry(context.Background(), 0, nil)
			}
		}(i)
	}
	wg.Wait()

	settler.mu.Lock()
	total := settler.acceptCalls + settler.releaseCalls + settler.modifyCalls
	settler.mu.Unlock()

	if total != 1 {
		t.Fatalf("expected exactly 1 settlement operation, got %d (accept=%d, release=%d, modify=%d)",
			total, settler.acceptCalls, settler.releaseCalls, settler.modifyCalls)
	}
}

// TestDelivery_Extend_NotSupported validates Extend returns ErrNotSupported.
func TestDelivery_Extend_NotSupported(t *testing.T) {
	settler := newMockSettler()
	env := &domain.Envelope{ID: "extend-test"}
	msg := &amqp.Message{}
	d := NewDelivery(env, msg, settler, slog.Default(), &ports.NoopExporter{}, nil)

	err := d.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Extend() = %v, want ErrNotSupported", err)
	}
}
