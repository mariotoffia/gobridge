package paho

import (
	"context"
	"fmt"

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
}

var _ ports.Sender = (*Sender)(nil)

// NewSender creates a Sender bound to the given Session.
func NewSender(session *Session, opts SenderOptions) *Sender {
	return &Sender{session: session, opts: opts}
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

	// Apply configured timeout if set.
	if s.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.Timeout)
		defer cancel()
	}

	resp, err := cm.Publish(ctx, pub)
	if err != nil {
		return MapError(err)
	}

	if resp != nil && resp.ReasonCode != 0 {
		if berr := MapPublishReasonCode(resp.ReasonCode); berr != nil {
			return berr.With("topic", topic).
				With("reason_code", fmt.Sprintf("0x%02X", resp.ReasonCode))
		}
	}

	return nil
}
