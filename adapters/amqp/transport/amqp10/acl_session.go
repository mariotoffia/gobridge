package amqp10

import (
	"context"
	"fmt"

	"github.com/Azure/go-amqp"
)

// amqpSessionLink wraps a *amqp.Session, exposing only the operations
// the adapter needs in domain-typed (wrapper) form. Receivers and
// Senders consume this wrapper, never the SDK type.
type amqpSessionLink struct {
	raw *amqp.Session
}

// Close closes the underlying AMQP session.
func (s *amqpSessionLink) Close(ctx context.Context) error {
	if s == nil || s.raw == nil {
		return nil
	}
	if err := s.raw.Close(ctx); err != nil {
		return fmt.Errorf("amqp10: session close: %w", err)
	}
	return nil
}

// NewReceiverLink opens a new receiver link on this session for the
// given address. Durability and capabilities are configured from
// ReceiverConfig fields without leaking SDK types to callers.
func (s *amqpSessionLink) NewReceiverLink(
	ctx context.Context,
	address string,
	credit int32,
	durabilityMode uint32,
	capability string,
) (*receiverLink, error) {
	opts := &amqp.ReceiverOptions{
		Credit:             credit,
		SourceCapabilities: []string{capability},
	}
	if durabilityMode > 0 {
		opts.Durability = amqp.Durability(durabilityMode)
	}
	r, err := s.raw.NewReceiver(ctx, address, opts)
	if err != nil {
		return nil, fmt.Errorf("amqp10: new receiver: %w", err)
	}
	return &receiverLink{raw: r}, nil
}

// NewSenderLink opens a new sender link on this session for the given
// address.
func (s *amqpSessionLink) NewSenderLink(
	ctx context.Context,
	address string,
	durabilityMode uint32,
	capability string,
) (*senderLink, error) {
	opts := &amqp.SenderOptions{
		TargetCapabilities: []string{capability},
	}
	if durabilityMode > 0 {
		opts.Durability = amqp.Durability(durabilityMode)
	}
	snd, err := s.raw.NewSender(ctx, address, opts)
	if err != nil {
		return nil, fmt.Errorf("amqp10: new sender: %w", err)
	}
	return &senderLink{raw: snd}, nil
}
