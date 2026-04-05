package paho

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG RES-002: MQTT Sender no fallback timeout when Timeout=0
//
// When SenderOptions.Timeout is 0 and the caller's context has no deadline,
// Send would run with no timeout at all. The fix adds a 60s safety-net
// fallback when Timeout<=0 and the context has no deadline.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRES002_Sender_FallbackTimeout_Applied verifies that when
// SenderOptions.Timeout is 0, the Send method still applies a deadline
// to the context (the 60s safety-net fallback).
func TestBugRES002_Sender_FallbackTimeout_Applied(t *testing.T) {
	s := &Sender{
		opts:    SenderOptions{Timeout: 0},
		metrics: &noopTestExporter{},
	}

	// applyTimeout is the internal helper extracted from Send.
	// We test the context transformation directly.
	ctx := context.Background()
	got, cancel1 := s.applyTimeout(ctx)
	defer cancel1()

	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("expected a deadline on the context when Timeout=0, but none was set")
	}

	remaining := time.Until(deadline)
	// The fallback is 60s; allow some margin for execution time.
	if remaining < 55*time.Second || remaining > 65*time.Second {
		t.Fatalf("expected ~60s deadline, got %v", remaining)
	}
}

// TestBugRES002_Sender_ExplicitTimeout_Applied verifies that when
// SenderOptions.Timeout is set, that value is used.
func TestBugRES002_Sender_ExplicitTimeout_Applied(t *testing.T) {
	s := &Sender{
		opts:    SenderOptions{Timeout: 5 * time.Second},
		metrics: &noopTestExporter{},
	}

	ctx := context.Background()
	got, cancel1 := s.applyTimeout(ctx)
	defer cancel1()

	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("expected a deadline on the context, but none was set")
	}

	remaining := time.Until(deadline)
	if remaining < 3*time.Second || remaining > 7*time.Second {
		t.Fatalf("expected ~5s deadline, got %v", remaining)
	}
}

// TestBugRES002_Sender_ExistingDeadline_NotOverridden verifies that
// when the context already has a deadline, no additional timeout is applied.
func TestBugRES002_Sender_ExistingDeadline_NotOverridden(t *testing.T) {
	s := &Sender{
		opts:    SenderOptions{Timeout: 0},
		metrics: &noopTestExporter{},
	}

	// Set a short deadline on the parent context.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, cancel2 := s.applyTimeout(ctx)
	defer cancel2()

	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("expected a deadline on the context")
	}

	remaining := time.Until(deadline)
	// Should still be ~2s (the original), not 60s.
	if remaining > 3*time.Second {
		t.Fatalf("expected existing ~2s deadline to be preserved, got %v", remaining)
	}
}

// noopTestExporter is a minimal no-op for test setup.
type noopTestExporter struct{}

func (n *noopTestExporter) Counter(string, int64, ...domain.Tag)          {}
func (n *noopTestExporter) Gauge(string, float64, ...domain.Tag)          {}
func (n *noopTestExporter) Histogram(string, float64, ...domain.Tag)      {}
func (n *noopTestExporter) Timer(string, time.Duration, ...domain.Tag)    {}
func (n *noopTestExporter) Flush(context.Context) error                   { return nil }
func (n *noopTestExporter) Close(context.Context) error                   { return nil }
