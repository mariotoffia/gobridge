package amqp091

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.SessionFactory  = (*Factory)(nil)
	_ ports.ReceiverFactory = (*ReceiverFactory)(nil)
	_ ports.SenderFactory   = (*SenderFactory)(nil)
)

// Factory implements ports.SessionFactory for AMQP 0-9-1.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

// NewSession creates an AMQP 0-9-1 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	opts, err := SessionOptionsFromMap(spec.Options)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 session %q: %s", spec.ID, err))
	}
	if err := opts.validate(); err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 session %q: %s", spec.ID, err))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// ReceiverFactory implements ports.ReceiverFactory for AMQP 0-9-1.
type ReceiverFactory struct {
	logger *slog.Logger
}

// NewReceiverFactory creates a ReceiverFactory.
func NewReceiverFactory(logger *slog.Logger) *ReceiverFactory {
	return &ReceiverFactory{logger: logger}
}

// NewReceiver creates an AMQP 0-9-1 Receiver bound to the given Session.
func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 receiver %q: session must be a non-nil AMQP 0-9-1 session", spec.ID))
	}

	cfg := ReceiverConfigFromOptions(spec.Options)
	cfg.Session = amqpSession
	cfg.Logger = f.logger

	if cfg.QueueName == "" && len(spec.Subscriptions) > 0 {
		cfg.QueueName = spec.Subscriptions[0].Topic
	}

	return NewReceiver(cfg), nil
}

// SenderFactory implements ports.SenderFactory for AMQP 0-9-1.
type SenderFactory struct {
	logger *slog.Logger
}

// NewSenderFactory creates a SenderFactory.
func NewSenderFactory(logger *slog.Logger) *SenderFactory {
	return &SenderFactory{logger: logger}
}

// NewSender creates an AMQP 0-9-1 Sender bound to the given Session.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: session must be a non-nil AMQP 0-9-1 session", spec.ID))
	}

	cfg := SenderConfigFromOptions(spec.Options)
	cfg.Session = amqpSession
	cfg.Logger = f.logger

	return NewSender(cfg), nil
}
