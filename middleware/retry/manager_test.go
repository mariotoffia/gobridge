package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// ═══════════════════════════════════════════════════════════════════════════
// Retry Manager Unit Tests
//
// Tests for the in-memory retry manager covering:
// - Message enqueueing and retry scheduling
// - Backoff calculation
// - Retry exhaustion and DLQ handling
// - Statistics tracking
// ═══════════════════════════════════════════════════════════════════════════

// TestMemoryRetryManager_Enqueue validates basic message enqueueing.
func TestMemoryRetryManager_Enqueue(t *testing.T) {
	policy := types.RetryPolicy{
		MaxAttempts:       3,
		InitialBackoff:    10 * time.Millisecond,
		MaxBackoff:        100 * time.Millisecond,
		BackoffMultiplier: 2.0,
	}

	dlq := NewMemoryDLQ()
	manager := NewMemoryRetryManager(policy, dlq)

	ctx := context.Background()
	msg := types.Message{Topic: "test", Payload: []byte("test message")}

	err := manager.Enqueue(ctx, msg, errors.New("test error"))
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	stats := manager.Stats()
	if stats.Pending != 1 {
		t.Errorf("expected 1 pending, got %d", stats.Pending)
	}
	if stats.TotalAttempts != 1 {
		t.Errorf("expected 1 total attempt, got %d", stats.TotalAttempts)
	}
}

// TestMemoryRetryManager_RetrySuccess validates successful retry processing.
func TestMemoryRetryManager_RetrySuccess(t *testing.T) {
	policy := types.RetryPolicy{
		MaxAttempts:       3,
		InitialBackoff:    1 * time.Millisecond,
		MaxBackoff:        10 * time.Millisecond,
		BackoffMultiplier: 2.0,
	}

	dlq := NewMemoryDLQ()
	manager := NewMemoryRetryManager(policy, dlq)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Enqueue a message
	msg := types.Message{Topic: "test", Payload: []byte("test")}
	err := manager.Enqueue(ctx, msg, errors.New("initial failure"))
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// Start processing with a handler that succeeds
	var processed atomic.Int32
	handler := types.SubscriberAdapter(func(ctx context.Context, topic string, payload types.Message) error {
		processed.Add(1)
		return nil
	})

	err = manager.Start(ctx, handler)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for processing
	time.Sleep(200 * time.Millisecond)
	manager.Stop()

	if processed.Load() != 1 {
		t.Errorf("expected 1 processed message, got %d", processed.Load())
	}

	stats := manager.Stats()
	if stats.Succeeded != 1 {
		t.Errorf("expected 1 succeeded, got %d", stats.Succeeded)
	}
}

// TestMemoryRetryManager_RetryExhaustion validates DLQ handling after retry exhaustion.
func TestMemoryRetryManager_RetryExhaustion(t *testing.T) {
	policy := types.RetryPolicy{
		MaxAttempts:       2,
		InitialBackoff:    1 * time.Millisecond,
		MaxBackoff:        5 * time.Millisecond,
		BackoffMultiplier: 2.0,
	}

	dlq := NewMemoryDLQ()
	manager := NewMemoryRetryManager(policy, dlq)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Enqueue a message
	msg := types.Message{Topic: "test", Payload: []byte("test")}
	err := manager.Enqueue(ctx, msg, errors.New("failure 1"))
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}

	// Start processing with a handler that always fails
	handler := types.SubscriberAdapter(func(ctx context.Context, topic string, payload types.Message) error {
		return errors.New("still failing")
	})

	err = manager.Start(ctx, handler)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for processing and exhaustion
	time.Sleep(500 * time.Millisecond)
	manager.Stop()

	// Check DLQ
	count, err := dlq.Count(ctx)
	if err != nil {
		t.Fatalf("DLQ count failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 message in DLQ, got %d", count)
	}

	stats := manager.Stats()
	if stats.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", stats.Failed)
	}
}

// TestMemoryRetryManager_Purge validates queue purging.
func TestMemoryRetryManager_Purge(t *testing.T) {
	policy := types.DefaultRetryPolicy()
	manager := NewMemoryRetryManager(policy, nil)

	ctx := context.Background()

	// Enqueue multiple messages
	for i := 0; i < 5; i++ {
		msg := types.Message{Topic: "test"}
		err := manager.Enqueue(ctx, msg, errors.New("error"))
		if err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}
	}

	if manager.Stats().Pending != 5 {
		t.Errorf("expected 5 pending, got %d", manager.Stats().Pending)
	}

	err := manager.Purge(ctx)
	if err != nil {
		t.Fatalf("Purge failed: %v", err)
	}

	if manager.Stats().Pending != 0 {
		t.Errorf("expected 0 pending after purge, got %d", manager.Stats().Pending)
	}
}

// TestBackoff_ExponentialWithJitter validates backoff calculation.
func TestBackoff_ExponentialWithJitter(t *testing.T) {
	policy := types.RetryPolicy{
		InitialBackoff:    100 * time.Millisecond,
		MaxBackoff:        10 * time.Second,
		BackoffMultiplier: 2.0,
		Jitter:            0.1,
	}

	tests := []struct {
		attempt     int
		minExpected time.Duration
		maxExpected time.Duration
	}{
		{1, 90 * time.Millisecond, 110 * time.Millisecond},
		{2, 180 * time.Millisecond, 220 * time.Millisecond},
		{3, 360 * time.Millisecond, 440 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			backoff := calculateBackoff(policy, tt.attempt)

			if backoff < tt.minExpected || backoff > tt.maxExpected {
				t.Errorf("attempt %d: backoff %v not in range [%v, %v]",
					tt.attempt, backoff, tt.minExpected, tt.maxExpected)
			}
		})
	}
}

// TestBackoff_MaxBackoffCap validates that backoff is capped at MaxBackoff.
func TestBackoff_MaxBackoffCap(t *testing.T) {
	policy := types.RetryPolicy{
		InitialBackoff:    time.Second,
		MaxBackoff:        5 * time.Second,
		BackoffMultiplier: 10.0,
		Jitter:            0,
	}

	// Attempt 3 would be 1s * 10^2 = 100s, but should be capped at 5s
	backoff := calculateBackoff(policy, 3)

	if backoff != 5*time.Second {
		t.Errorf("expected backoff capped at 5s, got %v", backoff)
	}
}

// TestMemoryDLQ_SendAndConsume validates DLQ operations.
func TestMemoryDLQ_SendAndConsume(t *testing.T) {
	dlq := NewMemoryDLQ()
	ctx := context.Background()

	// Send messages
	for i := 0; i < 3; i++ {
		msg := types.Message{Topic: "test"}
		err := dlq.Send(ctx, msg, errors.New("test error"))
		if err != nil {
			t.Fatalf("Send failed: %v", err)
		}
	}

	// Check count
	count, err := dlq.Count(ctx)
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 messages, got %d", count)
	}

	// Consume
	ch, err := dlq.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume failed: %v", err)
	}

	consumed := 0
	for range ch {
		consumed++
	}

	if consumed != 3 {
		t.Errorf("expected to consume 3 messages, got %d", consumed)
	}
}

// TestMemoryDLQ_Purge validates DLQ purging.
func TestMemoryDLQ_Purge(t *testing.T) {
	dlq := NewMemoryDLQ()
	ctx := context.Background()

	// Send messages
	for i := 0; i < 5; i++ {
		msg := types.Message{Topic: "test"}
		_ = dlq.Send(ctx, msg, errors.New("error"))
	}

	err := dlq.Purge(ctx)
	if err != nil {
		t.Fatalf("Purge failed: %v", err)
	}

	count, _ := dlq.Count(ctx)
	if count != 0 {
		t.Errorf("expected 0 after purge, got %d", count)
	}
}
