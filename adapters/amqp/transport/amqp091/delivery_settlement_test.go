// ═══════════════════════════════════════════════
// Delivery Settlement Bug Tests (amqp091)
//
// Validates BUG-2: sync.Once swallows settlement failure.
// When Ack fails, a subsequent Retry should not silently
// return nil.
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

// TestDelivery091_AckFails_ThenRetry_ReportsError exposes BUG-2 in amqp091.
func TestDelivery091_AckFails_ThenRetry_ReportsError(t *testing.T) {
	acker := newMockAcknowledger()
	acker.AckFn = func(uint64, bool) error {
		return errors.New("channel closed during ack")
	}

	env := &domain.Envelope{ID: "bug2-091"}
	raw := amqp.Delivery{
		Acknowledger: acker,
		DeliveryTag:  1,
	}
	d := NewDelivery(env, raw, slog.Default(), &ports.NoopExporter{}, nil)

	err1 := d.Ack(context.Background())
	if err1 == nil {
		t.Fatal("Ack() should have returned an error")
	}

	err2 := d.Retry(context.Background(), 0, nil)
	if err2 == nil {
		t.Fatal("Retry() after failed Ack() should not silently succeed (BUG-2)")
	}
}

// TestDelivery091_RetryFails_ThenAck_ReportsError validates reverse case.
func TestDelivery091_RetryFails_ThenAck_ReportsError(t *testing.T) {
	acker := newMockAcknowledger()
	acker.NackFn = func(uint64, bool, bool) error {
		return errors.New("channel closed during nack")
	}

	env := &domain.Envelope{ID: "bug2-091-rev"}
	raw := amqp.Delivery{
		Acknowledger: acker,
		DeliveryTag:  2,
	}
	d := NewDelivery(env, raw, slog.Default(), &ports.NoopExporter{}, nil)

	err1 := d.Retry(context.Background(), 0, nil)
	if err1 == nil {
		t.Fatal("Retry() should have returned an error")
	}

	err2 := d.Ack(context.Background())
	if err2 == nil {
		t.Fatal("Ack() after failed Retry() should not silently succeed (BUG-2)")
	}
}

// TestDelivery091_ConcurrentSettlement validates concurrent safety.
func TestDelivery091_ConcurrentSettlement(t *testing.T) {
	acker := newMockAcknowledger()
	env := &domain.Envelope{ID: "concurrent-091"}
	raw := amqp.Delivery{
		Acknowledger: acker,
		DeliveryTag:  3,
	}
	d := NewDelivery(env, raw, slog.Default(), &ports.NoopExporter{}, nil)

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

	acker.mu.Lock()
	total := acker.AckCalls + acker.NackCalls
	acker.mu.Unlock()

	if total != 1 {
		t.Fatalf("expected 1 settlement, got %d (ack=%d, nack=%d)",
			total, acker.AckCalls, acker.NackCalls)
	}
}

// TestDelivery091_Extend_NotSupported validates Extend.
func TestDelivery091_Extend_NotSupported(t *testing.T) {
	d, _ := makeTestDelivery(newMockAcknowledger(), 1)
	err := d.Extend(context.Background(), time.Now().Add(time.Minute))
	if !errors.Is(err, domain.ErrNotSupported) {
		t.Fatalf("Extend() = %v, want ErrNotSupported", err)
	}
}
