package amqp091

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.TransportFactory = (*Factory)(nil)
	_ ports.ReceiverFactory  = (*ReceiverFactory)(nil)
	_ ports.SenderFactory    = (*SenderFactory)(nil)
)

var errInvalidConfig = errors.New("amqp091: spec.Config must be of type amqp091.Config")

// Factory implements ports.TransportFactory for AMQP 0-9-1 (RabbitMQ).
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
		// amqp091 receivers subscribe (queue-declare + bind + consume) ONLY when
		// the session manager reconciles the SessionPlan, so a receiver on an
		// unmanaged session is silently inert; the builder enforces a manager
		// for these (ADV-P4-FU1).
		ports.CapPlanDrivenSubscriptions,
	}
}

// AddressValidator returns nil — AMQP 0-9-1 routing keys have no
// runtime-enforceable structural rules; the broker rejects unbound
// keys at publish time.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// NewSession creates an AMQP 0-9-1 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
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
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 receiver %q: session must be a non-nil AMQP 0-9-1 session", spec.ID))
	}
	var cfg Config
	if spec.Config != nil {
		c, err := configFromSpec(spec.Config)
		if err != nil {
			return nil, shared.ErrInvalidPayload.WithMessage(
				fmt.Sprintf("amqp091 receiver %q: %s", spec.ID, err))
		}
		cfg = c
	}
	// Defense in depth: the config decoder runs Config.Validate (which
	// rejects auto_ack for managed routes), but a programmatic spec may
	// bypass it. Re-reject here so the managed factory never builds a
	// receiver that broker-acks on delivery and loses messages on a
	// downstream failure.
	if cfg.Receiver.AutoAck {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 receiver %q: auto_ack=true is unsafe for a managed route; "+
				"remove it so deliveries settle at-least-once after the downstream succeeds", spec.ID))
	}
	cfg.Receiver.applyDefaults()
	rc := ReceiverConfig{
		QueueName:     cfg.Receiver.QueueName,
		ConsumerTag:   cfg.Receiver.ConsumerTag,
		AutoAck:       cfg.Receiver.AutoAck,
		Exclusive:     cfg.Receiver.Exclusive,
		PrefetchCount: cfg.Receiver.PrefetchCount,
		PrefetchSize:  cfg.Receiver.PrefetchSize,
		Session:       amqpSession,
		Logger:        f.logger,
	}
	if rc.QueueName == "" && len(spec.Subscriptions) > 0 {
		rc.QueueName = spec.Subscriptions[0].Topic
	}
	return NewReceiver(rc), nil
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
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: session must be a non-nil AMQP 0-9-1 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: %s", spec.ID, err))
	}
	// Defense in depth (see NewReceiver): reject the unsupported
	// basic.publish immediate flag even if the spec bypassed the decoder's
	// Config.Validate. RabbitMQ closes the channel when immediate is set.
	if cfg.Sender.Immediate {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: immediate=true is not supported by RabbitMQ; remove it", spec.ID))
	}
	sc := SenderConfig{
		Exchange:   cfg.Sender.Exchange,
		RoutingKey: cfg.Sender.RoutingKey,
		Mandatory:  cfg.Sender.Mandatory,
		Timeout:    cfg.Sender.Timeout,
		Session:    amqpSession,
		Logger:     f.logger,
	}
	return NewSender(sc), nil
}

// configFromSpec accepts both *Config and Config.
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
