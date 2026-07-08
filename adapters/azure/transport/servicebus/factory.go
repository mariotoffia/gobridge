package servicebus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.ReceiverFactory           = (*ReceiverFactory)(nil)
	_ ports.SenderFactory             = (*SenderFactory)(nil)
	_ ports.TransportFactory          = (*Factory)(nil)
	_ ports.VisibilityTimeoutProvider = (*Factory)(nil)
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

// NewReceiver creates a Service Bus Receiver from a ReceiverSpec. The
// receive entity is enforced here — the build boundary — because
// parse-time Validate also runs on binding overrides that legitimately
// omit it.
func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	if err := pc.ValidateReceiverEntity(); err != nil {
		return nil, fmt.Errorf("servicebus receiver %q: %w", spec.ID, err)
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

// NewSender creates a Service Bus Sender from a SenderSpec. Entity
// enforcement mirrors NewReceiver.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	if err := pc.ValidateSenderEntity(); err != nil {
		return nil, fmt.Errorf("servicebus sender %q: %w", spec.ID, err)
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

// Capabilities returns the transport-WIDE capabilities for Azure Service
// Bus. These describe the PeekLock (default) source, matching the SQS
// transport's set:
//
//   - CapVisibilityExtension — RenewMessageLock (Extend).
//   - CapSourceRedelivery    — AbandonMessage / lock expiry redelivers.
//   - CapDelayedSend         — ScheduleMessages backs a delayed Retry.
//
// A ReceiveAndDelete receiver honours NONE of these (the message is gone
// at receive time), and a topic subscription cannot honour a delayed
// Retry. The stateless Factory has no per-route config in scope, so it
// advertises the default PeekLock-queue set; the builder OVERRIDES it per
// route with Config.Capabilities() when the receiver config implements
// ports.CapabilityConfig (bridge/builder_complete.go).
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
		ports.CapDelayedSend,
	}
}

// AddressValidator returns nil — Azure Service Bus entity names are
// validated by the SDK at send time and have no runtime-enforceable
// rendered-address rules of the kind MQTT topics need.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// VisibilityTimeout returns the default Service Bus message lock
// duration (30 s) — the ASB analog of a visibility window and the
// safe-by-default counterpart to ReceiverConfig.LockDuration's own
// default. The runtime validator uses it to reject a SendTimeout that
// exceeds half the window, which would risk duplicate processing.
//
// The Factory is stateless and has no per-entity LockDuration in scope,
// so this is a conservative constant rather than a per-receiver value.
// A route configured with a LONGER lock only gains slack. A route with a
// SHORTER lock (ASB allows down to 5 s) is threaded through instead via
// Config.EffectiveVisibilityTimeout() (ports.VisibilityTimeoutConfig),
// which the builder prefers over this constant (D2, Phase 1b), mirroring
// the identical SQS boundary. This method remains the fallback when no
// receiver config is available.
func (f *Factory) VisibilityTimeout() time.Duration {
	return 30 * time.Second
}
