package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
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
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapStatefulSession}
}

// NewSession creates an AMQP 1.0 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
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
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: %s", spec.ID, err))
	}
	rc := ReceiverConfig{
		Address:        cfg.Receiver.Address,
		LinkCredit:     cfg.Receiver.LinkCredit,
		DurabilityMode: cfg.Receiver.DurabilityMode,
		Routing:        cfg.Receiver.Routing,
		Logger:         f.Logger,
		Metrics:        f.Metrics,
		Session:        amqpSession,
	}
	return NewReceiver(rc, amqpSession)
}

// NewSender creates an AMQP 1.0 Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: %s", spec.ID, err))
	}
	sc := SenderConfig{
		Address:        cfg.Sender.Address,
		Timeout:        cfg.Sender.Timeout,
		DurabilityMode: cfg.Sender.DurabilityMode,
		Routing:        cfg.Sender.Routing,
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
