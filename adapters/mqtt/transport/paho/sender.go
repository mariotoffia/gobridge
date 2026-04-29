package paho

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Sender implements ports.Sender for MQTT publishing.
//
// For QoS 1, Send blocks until PUBACK is received from the broker.
// For QoS 2, Send blocks until PUBCOMP is received.
// This means a nil return from Send confirms broker acceptance.
type Sender struct {
	session *Session
	opts    SenderOptions
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

var _ ports.Sender = (*Sender)(nil)

// NewSender creates a Sender bound to the given Session.
func NewSender(session *Session, opts SenderOptions) *Sender {
	m := session.metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	return &Sender{session: session, opts: opts, logger: session.logger, metrics: m}
}

// Send publishes the envelope to the MQTT broker. The topic is taken from
// env.Subject; if empty, opts.DefaultTopic is used. Headers are mapped to
// MQTT 5 user properties. Message expiry is derived from env.RemainingTTL().
//
// Returns nil when the broker has accepted the message (PUBACK / PUBCOMP).
// Returns a classified domain.BridgeError on failure.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	if env == nil {
		return domain.ErrInvalidPayload.WithMessage("nil envelope")
	}

	cm := s.session.ConnectionManager()
	if cm == nil {
		return domain.ErrUnavailable.WithMessage("MQTT session not connected")
	}

	pub := PublishFromEnvelope(env, s.opts)

	topic := pub.Topic
	if topic == "" {
		return domain.ErrInvalidTopic.WithMessage("no topic specified and no default topic configured")
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: publishing",
			"topic", topic,
			"qos", pub.QoS,
			"payload_len", len(env.Payload),
			"envelope_id", env.ID,
		)
	}

	// Apply timeout: use configured value, or a 60s safety-net fallback
	// when Timeout<=0 and the caller's context has no deadline.
	ctx, timeoutCancel := s.applyTimeout(ctx)
	defer timeoutCancel()

	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: s.session.opts.ClientID}

	start := time.Now()
	resp, err := cm.Publish(ctx, pub)
	elapsed := time.Since(start)

	if err != nil {
		s.metrics.Timer(domain.MetricMQTTPublishLatency, elapsed, sessionTag)
		s.metrics.Counter(domain.MetricMQTTPublishFailures, 1, sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: publish failed",
				"topic", topic, "error", err)
		}
		return MapError(err)
	}

	if resp != nil && resp.ReasonCode != 0 {
		if berr := MapPublishReasonCode(resp.ReasonCode); berr != nil {
			s.metrics.Timer(domain.MetricMQTTPublishLatency, elapsed, sessionTag)
			s.metrics.Counter(domain.MetricMQTTPublishFailures, 1, sessionTag)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug, "mqtt: publish rejected",
					"topic", topic,
					"reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
			}
			result := berr.With("topic", topic).
				With("reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
			// Hint the route runner to back off on quota/throttle errors.
			if resp.ReasonCode == 0x93 || resp.ReasonCode == 0x97 || resp.ReasonCode == 0xA1 {
				hint := s.opts.ThrottleRetryAfter
				if hint <= 0 {
					hint = 500 * time.Millisecond
				}
				result = result.WithRetryAfter(hint)
			}
			return result
		}
	}

	s.metrics.Timer(domain.MetricMQTTPublishLatency, elapsed, sessionTag)

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: published",
			"topic", topic,
			"envelope_id", env.ID,
			"duration", elapsed,
		)
	}

	return nil
}

// defaultSendTimeout is the safety-net fallback used when
// SenderOptions.Timeout is zero and the context has no deadline.
const defaultSendTimeout = 60 * time.Second

// applyTimeout returns a context with a deadline derived from the
// configured Timeout. When Timeout <= 0 the fallback of 60 s is used.
// If the context already carries a deadline, no additional timeout is
// applied so we do not accidentally shorten an existing one.
func (s *Sender) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = defaultSendTimeout
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {} // noop cancel — caller's deadline is already shorter
}
