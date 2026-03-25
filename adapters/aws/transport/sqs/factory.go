package sqs

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time checks.
var (
	_ ports.ReceiverFactory = (*ReceiverFactory)(nil)
	_ ports.SenderFactory   = (*SenderFactory)(nil)
)

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
type SenderFactory struct{}

// NewSenderFactory returns a factory that creates SQS senders.
func NewSenderFactory() *SenderFactory {
	return &SenderFactory{}
}

// NewSender creates a Sender from a SenderSpec. SQS is stateless so
// the session parameter is ignored.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	cfg := SenderConfigFromOptions(spec.Options)
	return NewSender(cfg)
}
