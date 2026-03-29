package paho

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
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
}

var _ ports.Sender = (*Sender)(nil)

// NewSender creates a Sender bound to the given Session.
func NewSender(session *Session, opts SenderOptions) *Sender {
	return &Sender{session: session, opts: opts, logger: session.logger}
}

// Send publishes the envelope to the MQTT broker. The topic is taken from
// env.Subject; if empty, opts.DefaultTopic is used. Headers are mapped to
// MQTT 5 user properties. Message expiry is derived from env.RemainingTTL().
//
// Returns nil when the broker has accepted the message (PUBACK / PUBCOMP).
// Returns a classified domain.BridgeError on failure.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
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

	// Apply configured timeout if set.
	if s.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	resp, err := cm.Publish(ctx, pub)
	if err != nil {
		logging.DebugContext(s.logger, ctx, "mqtt: publish failed",
			"topic", topic, "error", err)
		return MapError(err)
	}

	if resp != nil && resp.ReasonCode != 0 {
		if berr := MapPublishReasonCode(resp.ReasonCode); berr != nil {
			logging.DebugContext(s.logger, ctx, "mqtt: publish rejected",
				"topic", topic,
				"reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
			return berr.With("topic", topic).
				With("reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
		}
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: published",
			"topic", topic,
			"envelope_id", env.ID,
			"duration", time.Since(start),
		)
	}

	return nil
}
