package sqs

import (
	"context"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time checks.
var (
	_ ports.ReceiverFactory           = (*ReceiverFactory)(nil)
	_ ports.SenderFactory             = (*SenderFactory)(nil)
	_ ports.TransportFactory          = (*Factory)(nil)
	_ ports.VisibilityTimeoutProvider = (*Factory)(nil)
)

// Factory is the SQS transport factory. SQS is stateless: NewSession
// returns (nil, nil) and the session parameter passed to
// NewReceiver/NewSender is ignored.
type Factory struct {
	recv *ReceiverFactory
	send *SenderFactory
}

// NewFactory creates a stateless SQS TransportFactory.
func NewFactory(logger *slog.Logger) *Factory {
	return &Factory{
		recv: NewReceiverFactory(logger),
		send: NewSenderFactory(logger),
	}
}

// NewSession returns (nil, nil) — SQS does not maintain a session.
func (f *Factory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, nil
}

// NewReceiver delegates to the inner ReceiverFactory.
func (f *Factory) NewReceiver(ctx context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	return f.recv.NewReceiver(ctx, spec, session)
}

// NewSender delegates to the inner SenderFactory.
func (f *Factory) NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	return f.send.NewSender(ctx, spec, session)
}

// Capabilities returns SQS transport capabilities: visibility extension
// (for delivery lock renewal) and source redelivery (SQS automatically
// redelivers un-deleted messages).
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
	}
}

// VisibilityTimeout returns the default SQS visibility timeout (30s).
// The runtime validator uses this to check that SendTimeout does not
// exceed half the visibility window.
func (f *Factory) VisibilityTimeout() time.Duration {
	return 30 * time.Second
}

// ReceiverFactory creates SQS Receiver instances from ReceiverSpec.
type ReceiverFactory struct {
	logger *slog.Logger
}

// NewReceiverFactory returns a factory that creates SQS receivers.
func NewReceiverFactory(logger *slog.Logger) *ReceiverFactory {
	return &ReceiverFactory{logger: logger}
}

// NewReceiver creates a Receiver from a ReceiverSpec. SQS is stateless
// so the session parameter is ignored.
func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	cfg := ReceiverConfigFromOptions(spec.Options)
	return NewReceiver(cfg, f.logger)
}

// SenderFactory creates SQS Sender instances from SenderSpec.
type SenderFactory struct {
	logger *slog.Logger
}

// NewSenderFactory returns a factory that creates SQS senders.
func NewSenderFactory(logger *slog.Logger) *SenderFactory {
	return &SenderFactory{logger: logger}
}

// NewSender creates a Sender from a SenderSpec. SQS is stateless so
// the session parameter is ignored.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	cfg := SenderConfigFromOptions(spec.Options)
	cfg.Logger = f.logger
	return NewSender(cfg)
}
