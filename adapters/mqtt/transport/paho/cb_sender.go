package paho

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// CircuitBreakerSender wraps a Sender with circuit breaker protection.
// When the breaker opens, Send returns the breaker's rejection error
// (a *shared.BridgeError carrying ErrUnavailable + RetryAfter) rather
// than attempting the broker publish.
type CircuitBreakerSender struct {
	inner   *Sender
	breaker ports.CircuitBreaker
	logger  *slog.Logger
	metrics ports.MetricsExporter
	// sessionID is captured at construction so circuit-open metrics carry
	// the same session_id tag that Sender.Send emits on every other
	// failure path (ensures consistent metric labels for correlation).
	sessionID string
}

var _ ports.Sender = (*CircuitBreakerSender)(nil)

// NewCircuitBreakerSender wraps the given sender with a circuit breaker.
// The breaker is supplied by the composition root so this adapter never
// imports the breaker implementation package — only the ports.CircuitBreaker
// contract.
func NewCircuitBreakerSender(inner *Sender, breaker ports.CircuitBreaker) *CircuitBreakerSender {
	sessionID := ""
	if inner.session != nil {
		sessionID = inner.session.opts.ClientID
	}

	var logger *slog.Logger
	if inner.logger != nil {
		logger = inner.logger
	}

	return &CircuitBreakerSender{
		inner:     inner,
		breaker:   breaker,
		logger:    logger,
		metrics:   inner.metrics,
		sessionID: sessionID,
	}
}

// Send checks the circuit breaker state before delegating to the inner
// sender. Records the outcome for state transitions.
//
// On circuit-open rejection, the emitted MetricMQTTPublishFailures
// counter carries BOTH a reason=circuit_open tag AND the session_id
// tag — matching the tagging convention used by Sender.Send so that
// operators can correlate publish failures (whether broker-side or
// breaker-side) by session.
func (s *CircuitBreakerSender) Send(ctx context.Context, env *domain.Envelope) error {
	if err := s.breaker.BeforeRequest(); err != nil {
		s.metrics.Counter(shared.MetricMQTTPublishFailures, 1,
			shared.Tag{Key: "reason", Value: "circuit_open"},
			shared.Tag{Key: shared.TagKeySessionID, Value: s.sessionID},
		)
		return err
	}
	err := s.inner.Send(ctx, env)
	s.breaker.AfterRequest(err)
	return err
}
