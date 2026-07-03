package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.TransportFactory for AMQP 1.0.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var _ ports.TransportFactory = (*Factory)(nil)

var errInvalidConfig = errors.New("amqp10: spec.Config must be of type amqp10.Config")

// NewFactory creates an AMQP 1.0 transport factory.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{Logger: logger, Metrics: m}
}

// Capabilities returns the transport capabilities for AMQP 1.0.
//
// CapSourceRedelivery is declared because an AMQP 1.0 broker fully
// redelivers unsettled deliveries: releasing (Retry) or simply losing
// the link before settlement puts the message back on the source queue.
// Without this declaration the route validator treats amqp10 as a
// no-retry transport and forces a DLQ store or AllowRetryDrop even
// though the transport never drops.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapSourceRedelivery,
	}
}

// AddressValidator returns nil — AMQP 1.0 link target addresses have
// no runtime-enforceable rendered-address rules; the broker rejects
// invalid addresses on attach.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// NewSession creates an AMQP 1.0 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an AMQP 1.0 Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: %s", spec.ID, err))
	}
	// AMQP 1.0 receiver links are address-bound; enforced here — the
	// build boundary — because parse-time Validate also runs on binding
	// overrides that legitimately omit the address.
	if cfg.Receiver.Address == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: receiver.address is required", spec.ID))
	}
	rc := ReceiverConfig{
		Address:          cfg.Receiver.Address,
		LinkCredit:       cfg.Receiver.LinkCredit,
		DurabilityMode:   cfg.Receiver.DurabilityMode,
		Routing:          cfg.Receiver.Routing,
		SubscriptionName: cfg.Receiver.SubscriptionName,
		Logger:           f.Logger,
		Metrics:          f.Metrics,
		Session:          amqpSession,
	}
	return NewReceiver(rc, amqpSession)
}

// NewSender creates an AMQP 1.0 Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: %s", spec.ID, err))
	}
	// Sender links are address-bound; same build-boundary enforcement
	// as NewReceiver.
	if cfg.Sender.Address == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: sender.address is required", spec.ID))
	}
	sc := SenderConfig{
		Address:        cfg.Sender.Address,
		Timeout:        cfg.Sender.Timeout,
		DurabilityMode: cfg.Sender.DurabilityMode,
		Routing:        cfg.Sender.Routing,
		Durable:        cfg.Sender.Durable,
		Logger:         f.Logger,
		Metrics:        f.Metrics,
		Session:        amqpSession,
	}
	if sc.Timeout == 0 {
		sc.Timeout = DefaultSenderOptions().Timeout
	}
	return NewSender(sc, amqpSession)
}

// configFromSpec accepts both *Config and Config so registry-decoded
// pointers and hand-built test value fixtures both work.
func configFromSpec(pc ports.PluginConfig) (Config, error) {
	if pc == nil {
		return Config{}, errInvalidConfig
	}
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
