package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.TransportFactory for MQTT via Eclipse Paho.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var _ ports.TransportFactory = (*Factory)(nil)

var errInvalidConfig = errors.New("mqtt: spec.Config must be of type paho.Config")

// NewFactory creates an MQTT Paho transport factory.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{Logger: logger, Metrics: m}
}

// Capabilities returns the transport capabilities for MQTT.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapExclusiveIdentity,
	}
}

// NewSession creates an MQTT Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	if opts.ClientID == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: client_id is required", spec.ID))
	}
	if len(opts.BrokerURLs) == 0 {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: at least one broker URL is required", spec.ID))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an MQTT Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt receiver %q: session must be a non-nil MQTT session", spec.ID))
	}
	return NewReceiver(spec.ID, mqttSession), nil
}

// NewSender creates an MQTT Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: session must be a non-nil MQTT session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: %s", spec.ID, err))
	}
	opts := cfg.Sender
	if opts.QoS > 2 {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: qos must be 0, 1, or 2", spec.ID))
	}
	if opts.QoS == 0 {
		opts.QoS = DefaultSenderOptions().QoS
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultSenderOptions().Timeout
	}
	if opts.ThrottleRetryAfter == 0 {
		opts.ThrottleRetryAfter = DefaultSenderOptions().ThrottleRetryAfter
	}
	return NewSender(mqttSession, opts), nil
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
