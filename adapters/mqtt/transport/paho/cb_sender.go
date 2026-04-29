package paho

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// CBConfig configures the circuit breaker for an MQTT sender.
// All fields are optional; zero values use sensible defaults.
type CBConfig struct {
	FailureThreshold int           // consecutive failures before open (default 5)
	SuccessThreshold int           // consecutive successes in half-open before closing (default 2)
	ResetTimeout     time.Duration // time in open state before probing (default 30s)
}

func (c CBConfig) toCircuitBreakerConfig() circuitbreaker.Config {
	ft := c.FailureThreshold
	if ft <= 0 {
		ft = 5
	}
	st := c.SuccessThreshold
	if st <= 0 {
		st = 2
	}
	rt := c.ResetTimeout
	if rt <= 0 {
		rt = 30 * time.Second
	}
	return circuitbreaker.Config{
		FailureThreshold: ft,
		SuccessThreshold: st,
		ResetTimeout:     rt,
		CountError:       domain.IsRecoverableError,
	}
}

// CircuitBreakerSender wraps a Sender with circuit breaker protection.
// When the breaker opens, Send returns ErrUnavailable with a RetryAfter
// hint rather than attempting the broker publish.
type CircuitBreakerSender struct {
	inner   *Sender
	breaker *circuitbreaker.Breaker
	logger  *slog.Logger
	metrics ports.MetricsExporter
	// sessionID is captured at construction so circuit-open metrics carry
	// the same session_id tag that Sender.Send emits on every other
	// failure path (ensures consistent metric labels for correlation).
	sessionID string
}

var _ ports.Sender = (*CircuitBreakerSender)(nil)

// NewCircuitBreakerSender wraps the given sender with a circuit breaker.
func NewCircuitBreakerSender(inner *Sender, cfg CBConfig) *CircuitBreakerSender {
	cbCfg := cfg.toCircuitBreakerConfig()
	key := "mqtt-sender"
	sessionID := ""
	if inner.session != nil {
		sessionID = inner.session.opts.ClientID
		key = "mqtt-sender:" + sessionID
	}

	var logger *slog.Logger
	if inner.logger != nil {
		logger = inner.logger
	}

	return &CircuitBreakerSender{
		inner:     inner,
		breaker:   circuitbreaker.NewBreaker(key, cbCfg, nil),
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
		s.metrics.Counter(domain.MetricMQTTPublishFailures, 1,
			domain.Tag{Key: "reason", Value: "circuit_open"},
			domain.Tag{Key: domain.TagKeySessionID, Value: s.sessionID},
		)
		return err
	}
	err := s.inner.Send(ctx, env)
	s.breaker.AfterRequest(err)
	return err
}
