package amqp091

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

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

	// exclusiveSeen latches once this factory has built an exclusive
	// consumer. It drives the CapExclusiveIdentity advertisement so the
	// supervisor serializes (PrepareCommit) rather than overlaps old/new
	// instances on reconfig — overlapping exclusive consumers race a 403
	// ACCESS_REFUSED against the reconnect budget and can trip terminal
	// teardown. Latching (never cleared) is deliberately conservative:
	// once a route was exclusive, treat reconfig as identity-sensitive.
	exclusiveSeen atomic.Bool
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
		receivers: NewReceiverFactory(logger, m),
		senders:   NewSenderFactory(logger, m),
	}
}

// NewReceiver delegates to the inner ReceiverFactory.
func (f *Factory) NewReceiver(ctx context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	if f.receivers == nil {
		f.receivers = NewReceiverFactory(f.Logger, f.Metrics)
	}
	r, err := f.receivers.NewReceiver(ctx, spec, session)
	if err != nil {
		return nil, err
	}
	// Latch the exclusive-identity advertisement so a later reconfig is
	// serialized rather than overlapped (see Factory.exclusiveSeen).
	if rr, ok := r.(*Receiver); ok && rr.cfg.Exclusive {
		f.exclusiveSeen.Store(true)
	}
	return r, nil
}

// NewSender delegates to the inner SenderFactory.
func (f *Factory) NewSender(ctx context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	if f.senders == nil {
		f.senders = NewSenderFactory(f.Logger, f.Metrics)
	}
	return f.senders.NewSender(ctx, spec, session)
}

// Capabilities returns the transport capabilities for AMQP 0-9-1.
func (f *Factory) Capabilities() []ports.Capability {
	caps := []ports.Capability{
		ports.CapStatefulSession,
		ports.CapSourceRedelivery,
		// amqp091 receivers subscribe (queue-declare + bind + consume) ONLY when
		// the session manager reconciles the SessionPlan, so a receiver on an
		// unmanaged session is silently inert; the builder enforces a manager
		// for these (ADV-P4-FU1).
		ports.CapPlanDrivenSubscriptions,
	}
	// Advertise exclusive-identity once an exclusive consumer has been built
	// so the supervisor picks the serialized (PrepareCommit) swap mode: an
	// Overlap reconfig would run the old and new exclusive consumers against
	// the same queue concurrently, and the new one burns its 403 reconnect
	// budget waiting for the old to drain — tripping terminal teardown.
	if f.exclusiveSeen.Load() {
		caps = append(caps, ports.CapExclusiveIdentity)
	}
	return caps
}

// ConfigRequiresExclusiveIdentity reports whether the given RECEIVER plugin
// config declares an exclusive consumer, letting the supervisor pick the
// serialized swap mode on the FIRST reconfig that introduces exclusivity —
// before any receiver (and thus the exclusiveSeen latch) exists. Decode
// failure or nil cfg => false (no false positive).
//
// This complements — not replaces — the exclusiveSeen latch and the
// Capabilities() advertisement: the latch still covers the steady-state
// exclusive→exclusive reconfig where no config is available to inspect.
func (f *Factory) ConfigRequiresExclusiveIdentity(cfg ports.PluginConfig) bool {
	if cfg == nil {
		return false
	}
	c, err := configFromSpec(cfg)
	if err != nil {
		return false
	}
	return c.Receiver.Exclusive
}

// AddressValidator returns nil — AMQP 0-9-1 routing keys have no
// runtime-enforceable structural rules, so there is nothing to validate
// structurally at parse time.
//
// Note: contrary to a common assumption, the broker does NOT reject an
// unroutable routing key at publish time unless the publish sets
// mandatory=true. A non-mandatory publish (the default) to an exchange with
// no matching binding is confirmed and then SILENTLY DISCARDED — see the
// silent-drop warning on SenderConfig.Mandatory (config.go), doc.go, and the
// README. A nil AddressValidator therefore cannot catch an unbound key; only
// mandatory=true surfaces it (as a basic.return).
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
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

// NewReceiverFactory creates a ReceiverFactory. The optional metrics exporter
// is threaded into every built ReceiverConfig so managed receivers export the
// adapter's consume/settlement/reconnect metrics instead of silently dropping
// them to a NoopExporter (metrics-dropped finding). Omitting it (nil / no
// argument) keeps direct embedders defaulting to the NoopExporter.
func NewReceiverFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *ReceiverFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &ReceiverFactory{logger: logger, metrics: m}
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
		// Thread the factory's exporter through so managed receiver metrics
		// are actually exported (metrics-dropped finding); nil falls back to
		// the NoopExporter in NewReceiver.
		Metrics: f.metrics,
		// The managed route runner drains in-flight deliveries and then
		// calls Receiver.Close, so a graceful stop must HAND the channel
		// off (drain-then-close) rather than self-close it. Direct
		// embedders leave this false and self-close on graceful stop.
		deferCloseToRunner: true,
	}
	if rc.QueueName == "" && len(spec.Subscriptions) > 0 {
		rc.QueueName = spec.Subscriptions[0].Topic
	}
	// A consume on an empty queue name is a broker error at best;
	// enforced here — the build boundary — because parse-time Validate
	// also runs on binding overrides that legitimately omit the queue.
	if rc.QueueName == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 receiver %q: receiver.queue_name (or a subscription topic) is required", spec.ID))
	}
	return NewReceiver(rc), nil
}

// SenderFactory implements ports.SenderFactory for AMQP 0-9-1.
type SenderFactory struct {
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

// NewSenderFactory creates a SenderFactory. The optional metrics exporter is
// threaded into every built SenderConfig so managed senders export the
// adapter's publish/confirm/return metrics instead of silently dropping them
// to a NoopExporter (metrics-dropped finding). Omitting it (nil / no argument)
// keeps direct embedders defaulting to the NoopExporter.
func NewSenderFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *SenderFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &SenderFactory{logger: logger, metrics: m}
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
	// A managed sender must not silently lose unroutable publishes. With
	// mandatory=false the broker CONFIRMS an unroutable publish and then
	// DISCARDS it, so Send acks the source and the message vanishes with no
	// telemetry (mandatory-silent-drop finding). Require mandatory=true OR an
	// explicit allow_unroutable_drop opt-in that records the operator's
	// acceptance of that loss. Direct embedders that call NewSender with a raw
	// SenderConfig are unaffected — this gate is the managed build boundary.
	if !cfg.Sender.Mandatory && !cfg.Sender.AllowUnroutableDrop {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: mandatory=false silently drops unroutable publishes "+
				"(the broker confirms then discards them, so the source is acked and the message is lost); "+
				"set sender.mandatory=true to surface unroutable messages as basic.return, or "+
				"set sender.allow_unroutable_drop=true to deliberately accept the loss", spec.ID))
	}
	sc := SenderConfig{
		Exchange:     cfg.Sender.Exchange,
		RoutingKey:   cfg.Sender.RoutingKey,
		Mandatory:    cfg.Sender.Mandatory,
		DeliveryMode: cfg.Sender.DeliveryMode,
		Timeout:      cfg.Sender.Timeout,
		Session:      amqpSession,
		Logger:       f.logger,
		// Thread the factory's exporter through so managed sender metrics are
		// actually exported (metrics-dropped finding); nil falls back to the
		// NoopExporter in NewSender.
		Metrics: f.metrics,
	}
	if err := sc.validate(); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp091 sender %q: %s", spec.ID, err))
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
