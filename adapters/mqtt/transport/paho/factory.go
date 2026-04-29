package paho

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.TransportFactory for MQTT via Eclipse Paho.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var _ ports.TransportFactory = (*Factory)(nil)

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
	opts, err := SessionOptionsFromMap(spec.Options)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: %s", spec.ID, err))
	}

	if opts.ClientID == "" {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: client_id is required", spec.ID))
	}
	if len(opts.BrokerURLs) == 0 {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: at least one broker URL is required", spec.ID))
	}

	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an MQTT Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt receiver %q: session must be a non-nil MQTT session", spec.ID))
	}
	return NewReceiver(spec.ID, mqttSession), nil
}

// NewSender creates an MQTT Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: session must be a non-nil MQTT session", spec.ID))
	}

	opts, err := SenderOptionsFromMap(spec.Options)
	if err != nil {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: %s", spec.ID, err))
	}
	return NewSender(mqttSession, opts), nil
}
