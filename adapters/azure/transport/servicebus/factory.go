package servicebus

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.ReceiverFactory  = (*ReceiverFactory)(nil)
	_ ports.SenderFactory    = (*SenderFactory)(nil)
	_ ports.TransportFactory = (*Factory)(nil)
)

var errInvalidConfig = errors.New("servicebus: spec.Config must be of type servicebus.Config")

// ReceiverFactory creates Service Bus receivers from ports.ReceiverSpec.
type ReceiverFactory struct {
	logger *slog.Logger
}

// NewReceiverFactory returns a Service Bus ReceiverFactory.
func NewReceiverFactory(logger *slog.Logger) *ReceiverFactory {
	return &ReceiverFactory{logger: logger}
}

// NewReceiver creates a Service Bus Receiver from a ReceiverSpec.
func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	cfg := pc.toReceiverConfig()
	return NewReceiver(cfg, f.logger)
}

// SenderFactory creates Service Bus senders from ports.SenderSpec.
type SenderFactory struct {
	logger *slog.Logger
}

// NewSenderFactory returns a Service Bus SenderFactory.
func NewSenderFactory(logger *slog.Logger) *SenderFactory {
	return &SenderFactory{logger: logger}
}

// NewSender creates a Service Bus Sender from a SenderSpec.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	cfg := pc.toSenderConfig()
	cfg.Logger = f.logger
	return NewSender(cfg)
}

// configFromSpec accepts both *Config and Config so registry-decoded
// pointers and hand-built test value fixtures both work.
func configFromSpec(pc ports.PluginConfig) (Config, error) {
	switch v := pc.(type) {
	case *Config:
		if v == nil {
			return Config{}, errInvalidConfig
		}
		return *v, nil
	case Config:
		return v, nil
	default:
		return Config{}, errInvalidConfig
	}
}

// Factory is the Azure Service Bus transport factory. Service Bus is
// stateless from the bridge's perspective: NewSession returns
// (nil, nil) and the session parameter passed to NewReceiver/NewSender
// is ignored.
type Factory struct {
	recv *ReceiverFactory
	send *SenderFactory
}

// NewFactory creates a stateless Service Bus TransportFactory.
func NewFactory(logger *slog.Logger) *Factory {
	return &Factory{
		recv: NewReceiverFactory(logger),
		send: NewSenderFactory(logger),
	}
}

// NewSession returns (nil, nil) — Service Bus does not use sessions
// at the bridge layer.
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

// Capabilities returns the transport capabilities for Azure Service Bus.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapVisibilityExtension}
}

// AddressValidator returns nil — Azure Service Bus entity names are
// validated by the SDK at send time and have no runtime-enforceable
// rendered-address rules of the kind MQTT topics need.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }
