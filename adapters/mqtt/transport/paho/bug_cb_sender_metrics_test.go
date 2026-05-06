package paho

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG RES-008: CB Sender no metrics for circuit-open rejections
//
// When the circuit breaker is open and BeforeRequest returns an error,
// the Send method was returning immediately without emitting a publish-
// failure counter. The fix adds a metrics counter with reason=circuit_open.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRES008_CBSender_CircuitOpen_EmitsMetric verifies that when
// the circuit breaker rejects a Send because the circuit is open, a
// MetricMQTTPublishFailures counter with reason=circuit_open is emitted.
func TestBugRES008_CBSender_CircuitOpen_EmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}

	// Create a breaker and force it open by exceeding the failure threshold.
	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Minute, // long enough to stay open
		CountError:       func(error) bool { return true },
	}
	breaker := circuitbreaker.NewBreaker("test-cb", cfg, nil)

	// Trigger a failure to open the circuit.
	_ = breaker.BeforeRequest()
	breaker.AfterRequest(shared.ErrUnavailable)

	cbs := &CircuitBreakerSender{
		inner:   &Sender{metrics: rec},
		breaker: breaker,
		metrics: rec,
	}

	env := &messaging.Envelope{ID: "e1", Subject: "t/1", Payload: []byte("p")}
	err := cbs.Send(context.Background(), env)
	if err == nil {
		t.Fatal("expected error from open circuit, got nil")
	}

	entries := rec.FindEntries(shared.MetricMQTTPublishFailures)
	if len(entries) == 0 {
		t.Fatal("expected MetricMQTTPublishFailures counter when circuit is open, got none")
	}

	found := false
	for _, e := range entries {
		for _, tag := range e.Tags {
			if tag.Key == "reason" && tag.Value == "circuit_open" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected reason=circuit_open tag on the publish failure metric")
	}
}

// TestBugRES008_CBSender_CircuitClosed_NoExtraMetric verifies that when
// the circuit is closed, no circuit_open metric is emitted (the inner
// sender is called instead). We use a nil session to trigger a known
// error from the inner Send path.
func TestBugRES008_CBSender_CircuitClosed_NoExtraMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}

	cfg := circuitbreaker.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     30 * time.Second,
		CountError:       shared.IsRecoverableError,
	}
	breaker := circuitbreaker.NewBreaker("test-cb", cfg, nil)

	// Create a sender with no session -- Send will fail with ErrUnavailable
	// from the nil ConnectionManager check, not from the circuit breaker.
	inner := &Sender{
		session: &Session{},
		opts:    SenderOptions{Timeout: time.Second},
		metrics: rec,
	}

	cbs := &CircuitBreakerSender{
		inner:   inner,
		breaker: breaker,
		metrics: rec,
	}

	env := &messaging.Envelope{ID: "e2", Subject: "t/2", Payload: []byte("p")}
	_ = cbs.Send(context.Background(), env)

	// There should be no circuit_open failure metrics.
	entries := rec.FindEntries(shared.MetricMQTTPublishFailures)
	for _, e := range entries {
		for _, tag := range e.Tags {
			if tag.Key == "reason" && tag.Value == "circuit_open" {
				t.Fatal("should not emit circuit_open metric when circuit is closed")
			}
		}
	}
}
