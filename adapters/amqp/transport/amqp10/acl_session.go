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
//
// Durable subscriptions (durabilityMode > 0) require BOTH a durable
// SOURCE terminus (SourceDurability — state the BROKER retains) and a
// non-expiring source (SourceExpiryPolicy Never), plus a stable link
// name: brokers identify a durable subscription by container-id + link
// name, so a random per-attach name would orphan the old subscription
// and silently lose everything published while detached. The
// client-side Durability mirrors the mode for link-state symmetry.
func (s *amqpSessionLink) NewReceiverLink(
	ctx context.Context,
	address string,
	credit int32,
	durabilityMode uint32,
	capability string,
	linkName string,
) (*receiverLink, error) {
	opts := receiverLinkOptions(credit, durabilityMode, capability, linkName)
	r, err := s.raw.NewReceiver(ctx, address, opts)
	if err != nil {
		return nil, fmt.Errorf("amqp10: new receiver: %w", err)
	}
	return &receiverLink{raw: r}, nil
}

// receiverLinkOptions builds the SDK receiver options for a link. It is
// split out from NewReceiverLink so the negotiated settlement mode is
// unit-testable without a live broker.
//
// RequestedSenderSettleMode is pinned to SenderSettleModeUnsettled: left
// nil, go-amqp silently accepts whatever the broker offers, so a broker
// attaching snd-settle-mode=settled would deliver PRE-SETTLED messages
// (at-most-once) — a crash between receive and process would lose them
// while the adapter docs promise at-least-once. Requesting Unsettled
// makes a downgrading broker fail the attach LOUDLY instead of silently
// weakening the delivery guarantee.
func receiverLinkOptions(credit int32, durabilityMode uint32, capability, linkName string) *amqp.ReceiverOptions {
	opts := &amqp.ReceiverOptions{
		Credit:                    credit,
		SourceCapabilities:        []string{capability},
		RequestedSenderSettleMode: amqp.SenderSettleModeUnsettled.Ptr(),
	}
	if linkName != "" {
		opts.Name = linkName
	}
	if durabilityMode > 0 {
		opts.Durability = amqp.Durability(durabilityMode)
		opts.SourceDurability = amqp.Durability(durabilityMode)
		opts.SourceExpiryPolicy = amqp.ExpiryPolicyNever
	}
	return opts
}

// NewSenderLink opens a new sender link on this session for the given
// address. msgDurable sets the message-header durable flag stamped on
// every message sent over the returned link (see SenderConfig.Durable).
func (s *amqpSessionLink) NewSenderLink(
	ctx context.Context,
	address string,
	durabilityMode uint32,
	capability string,
	msgDurable bool,
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
	return &senderLink{raw: snd, durable: msgDurable}, nil
}
