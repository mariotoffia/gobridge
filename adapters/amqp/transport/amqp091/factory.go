package amqp091

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.TransportFactory = (*Factory)(nil)
	_ ports.ReceiverFactory  = (*ReceiverFactory)(nil)
	_ ports.SenderFactory    = (*SenderFactory)(nil)
)

// Factory implements ports.TransportFactory for AMQP 0-9-1 (RabbitMQ).
// RabbitMQ supports stateful sessions and source-level redelivery via
// nack+requeue.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter

	receivers *ReceiverFactory
	senders   *SenderFactory
}

// NewFactory creates an AMQP 0-9-1 transport factory.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{
		Logger:    logger,
		Metrics:   m,
		receivers: NewReceiverFactory(logger),
		senders:   NewSenderFactory(logger),
	}
}

// NewReceiver delegates to the inner ReceiverFactory.
func (f *Factory) NewReceiver(ctx context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	if f.receivers == nil {
		f.receivers = NewReceiverFactory(f.Logger)
	}
	return f.receivers.NewReceiver(ctx, spec, session)
}

// NewSender delegates to the inner SenderFactory.
func (f *Factory) NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	if f.senders == nil {
		f.senders = NewSenderFactory(f.Logger)
	}
	return f.senders.NewSender(ctx, spec, session)
}

// Capabilities returns the transport capabilities for AMQP 0-9-1.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapSourceRedelivery,
	}
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
