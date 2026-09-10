package sqs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

var errInvalidConfig = errors.New("sqs: spec.Config must be of type sqs.Config")

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

// NewFactory creates a stateless SQS TransportFactory. The optional
// variadic metrics exporter is threaded into every Receiver and Sender
// the factory builds so the adapter's twelve SQS metrics actually emit on
// the config-driven/plugin path — without it the factory left cfg.Metrics
// nil and each adapter fell back to a Noop exporter. The
// signature mirrors paho.NewFactory; the variadic keeps existing
// single-arg callers compiling.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{
		recv: NewReceiverFactory(logger, m),
		send: NewSenderFactory(logger, m),
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

// Capabilities returns SQS transport capabilities:
//
//   - CapVisibilityExtension: a Delivery can renew its visibility lock
//     (ChangeMessageVisibility) so direct_hold routes are admissible.
//   - CapSourceRedelivery: SQS automatically redelivers a message whose
//     visibility timeout lapses without a DeleteMessage.
//   - CapDelayedSend: SQS supports per-message DelaySeconds (0-900), so
//     the sender can honour delayed delivery requests.
//
// ponytail: CapSharedConsumer is deliberately OMITTED. SQS is a
// competing-consumer work queue — each message is delivered to exactly
// one consumer, and horizontal scaling across many pollers is the
// intended mode. CapSharedConsumer means a *broadcast* fan-out source
// that needs single-active fencing; runtime/validator.go:89 consumes it
// to REJECT any unfenced direct_hold route. Declaring it would force
// every SQS direct_hold route (e.g. scenario 02) to adopt a lease/outbox
// it does not need, defeating SQS's scale-out model. No built-in
// transport declares CapSharedConsumer; the omission is intentional, not
// an oversight.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapVisibilityExtension,
		ports.CapSourceRedelivery,
		ports.CapDelayedSend,
	}
}

// AddressValidator returns nil — SQS queue URLs/names are validated by
// the AWS SDK at send time and have no runtime-enforceable
// rendered-address rules of the kind MQTT topics need.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// VisibilityTimeout returns the default SQS visibility timeout (30s).
// The runtime validator uses this to check that SendTimeout does not
// exceed half the visibility window.
//
// This Factory is a stateless singleton with no access to the per-route
// receiver config, so it reports the transport-wide default. A route that
// sets e.g. visibility_timeout: 120 is threaded through instead via
// Config.EffectiveVisibilityTimeout() (ports.VisibilityTimeoutConfig),
// which the builder prefers over this constant (Phase 1b). This
// method remains the fallback for callers that provide no receiver config.
func (f *Factory) VisibilityTimeout() time.Duration {
	return 30 * time.Second
}

// ReceiverFactory creates SQS Receiver instances from ReceiverSpec.
type ReceiverFactory struct {
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

// NewReceiverFactory returns a factory that creates SQS receivers. The
// optional metrics exporter is applied to every ReceiverConfig so the
// receiver's poll/receive/malformed metrics emit. The
// variadic keeps existing single-arg callers compiling.
func NewReceiverFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *ReceiverFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &ReceiverFactory{logger: logger, metrics: m}
}

// NewReceiver creates a Receiver from a ReceiverSpec. SQS is stateless
// so the session parameter is ignored. The queue reference is enforced
// here — the build boundary — because parse-time Validate also runs on
// binding overrides that legitimately omit it.
func (f *ReceiverFactory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	if err := pc.ValidateQueue(); err != nil {
		return nil, fmt.Errorf("sqs receiver %q: %w", spec.ID, err)
	}
	cfg := pc.toReceiverConfig()
	cfg.Metrics = f.metrics
	return NewReceiver(cfg, f.logger)
}

// SenderFactory creates SQS Sender instances from SenderSpec.
type SenderFactory struct {
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

// NewSenderFactory returns a factory that creates SQS senders. The
// optional metrics exporter is applied to every SenderConfig so the
// sender's send/batch/dropped-attribute metrics emit. The
// variadic keeps existing single-arg callers compiling.
func NewSenderFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *SenderFactory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &SenderFactory{logger: logger, metrics: m}
}

// NewSender creates a Sender from a SenderSpec. SQS is stateless so
// the session parameter is ignored. Queue enforcement mirrors
// NewReceiver.
func (f *SenderFactory) NewSender(_ context.Context, spec ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	pc, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, err
	}
	if err := pc.ValidateQueue(); err != nil {
		return nil, fmt.Errorf("sqs sender %q: %w", spec.ID, err)
	}
	cfg := pc.toSenderConfig()
	cfg.Logger = f.logger
	cfg.Metrics = f.metrics
	// Surface the message-size ceiling to the config-driven (YAML) path:
	// pass the functional option ONLY when the operator set a positive value
	// so an absent/0 config keeps the 256 KiB default instead of zeroing the
	// ceiling (which would drop ALL attributes, including the rank-0
	// idempotency-key / traceparent headers).
	var opts []SenderOption
	if cfg.MaxMessageBytes > 0 {
		opts = append(opts, WithMaxMessageBytes(cfg.MaxMessageBytes))
	}
	return NewSender(cfg, opts...)
}

// configFromSpec accepts both *Config (the canonical decoder return)
// and Config (legacy hand-built test fixtures) and returns a value
// the projection helpers can use.
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
