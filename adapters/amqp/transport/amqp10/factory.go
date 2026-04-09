package amqp10

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.SessionFactory, ports.ReceiverFactory, and
// ports.SenderFactory for AMQP 1.0.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var (
	_ ports.SessionFactory  = (*Factory)(nil)
	_ ports.ReceiverFactory = (*Factory)(nil)
	_ ports.SenderFactory   = (*Factory)(nil)
)

// NewSession creates an AMQP 1.0 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	opts, err := SessionOptionsFromMap(spec.Options)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an AMQP 1.0 Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}

	cfg := ReceiverConfigFromOptions(spec.Options)
	cfg.Logger = f.Logger
	cfg.Metrics = f.Metrics
	cfg.Session = amqpSession

	return NewReceiver(cfg, amqpSession)
}

// NewSender creates an AMQP 1.0 Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}

	cfg := SenderConfigFromOptions(spec.Options)
	cfg.Logger = f.Logger
	cfg.Metrics = f.Metrics
	cfg.Session = amqpSession

	return NewSender(cfg, amqpSession)
}
