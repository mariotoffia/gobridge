package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// T2 Regression: TOCTOU race in pushEvent (hold mutex during send)
//
// These tests verify that pushEvent is safe to call concurrently with
// Close without panicking on a closed channel.
//
// ┌──────────────────────────────────────────────────────────────────────┐
// │  Before fix:                                                       │
// │    pushEvent: lock → check s.closed → unlock → send on s.events   │
// │    Close:     lock → s.closed=true → unlock → close(s.events)     │
// │    Race window: between pushEvent's unlock and channel send        │
// │    → panic: send on closed channel                                │
// │                                                                    │
// │  After fix:                                                        │
// │    pushEvent: lock → check s.closed → send on s.events → unlock   │
// │    Close cannot close(s.events) while pushEvent is sending         │
// │    → no panic                                                      │
// └──────────────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestPushEvent_ConcurrentClose_NoPanic validates that calling pushEvent
// concurrently with Close does not cause a panic on the closed channel.
// The test hammers both paths from multiple goroutines under the race
// detector.
//
// Assertions:
//   - No panic occurs
//   - All goroutines complete within the timeout
func TestPushEvent_ConcurrentClose_NoPanic(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		s := NewSession(
			SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
			domain.SessionEphemeral,
			nil,
		)

		var wg sync.WaitGroup
		const pushers = 10

		wg.Add(pushers + 1)

		for i := 0; i < pushers; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					s.pushEvent(ports.SessionConnected, nil)
				}
			}()
		}

		go func() {
			defer wg.Done()
			time.Sleep(50 * time.Microsecond)
			_ = s.Close(context.Background())
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: goroutines did not complete — deadlock suspected", trial)
		}
	}
}

// TestPushEvent_AfterClose_IsNoop validates that pushEvent called after
// Close is a silent no-op: no panic, no event delivered.
//
// Assertions:
//   - No panic
//   - Event channel is drained and closed
func TestPushEvent_AfterClose_IsNoop(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		domain.SessionEphemeral,
		nil,
	)

	_ = s.Close(context.Background())

	// Must not panic.
	s.pushEvent(ports.SessionConnected, nil)
	s.pushEvent(ports.SessionReconnecting, nil)
	s.pushEvent(ports.SessionDisconnected, nil)

	// Channel should be closed — reads return zero value immediately.
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("expected closed channel after Close, but got an event")
		}
	default:
		t.Fatal("expected closed channel to yield zero value, got nothing")
	}
}

// TestPushEvent_BufferFull_DropsOldest validates that when the event
// buffer is full, pushEvent drops the oldest event and inserts the new one.
//
// Scenario:
// ───────────────────────────────────────────────────────────────────────
//   Buffer capacity = 16
//   Push 16 events (types 0..15), filling the buffer
//   Push 1 more event (type SessionError)
//   The oldest event should be dropped; newest should be present
// ───────────────────────────────────────────────────────────────────────
//
// Assertions:
//   - The newest event (SessionError) is present in the channel
//   - Exactly 16 events are in the channel (one dropped, one added)
func TestPushEvent_BufferFull_DropsOldest(t *testing.T) {
	s := NewSession(
		SessionOptions{BrokerURLs: []string{"tcp://localhost:1883"}, ClientID: "test"},
		domain.SessionEphemeral,
		nil,
	)

	// Fill the buffer (capacity 16).
	for i := 0; i < 16; i++ {
		s.pushEvent(ports.SessionConnected, nil)
	}

	// Buffer is full; push one more. This should drop the oldest.
	s.pushEvent(ports.SessionError, nil)

	var events []ports.SessionEvent
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			events = append(events, ev)
		default:
			goto done
		}
	}
done:
	if len(events) != 16 {
		t.Fatalf("expected 16 events in buffer (one dropped, one added), got %d", len(events))
	}

	last := events[len(events)-1]
	if last.Type != ports.SessionError {
		t.Fatalf("expected last event to be SessionError, got %d", last.Type)
	}
}
