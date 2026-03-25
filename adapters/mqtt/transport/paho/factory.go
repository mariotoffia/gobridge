package paho

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.SessionFactory, ports.ReceiverFactory, and
// ports.SenderFactory for MQTT via Eclipse Paho.
type Factory struct {
	Logger *slog.Logger
}

var (
	_ ports.SessionFactory  = (*Factory)(nil)
	_ ports.ReceiverFactory = (*Factory)(nil)
	_ ports.SenderFactory   = (*Factory)(nil)
)

// NewSession creates an MQTT Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	opts := SessionOptionsFromMap(spec.Options)

	if opts.ClientID == "" {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: client_id is required", spec.ID))
	}
	if len(opts.BrokerURLs) == 0 {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: at least one broker URL is required", spec.ID))
	}

	return NewSession(opts, spec.SessionMode, f.Logger), nil
}

// NewReceiver creates an MQTT Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	mqttSession, ok := session.(*Session)
	if !ok {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt receiver %q: session must be an MQTT session", spec.ID))
	}
	return NewReceiver(spec.ID, mqttSession), nil
}

// NewSender creates an MQTT Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	mqttSession, ok := session.(*Session)
	if !ok {
		return nil, domain.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: session must be an MQTT session", spec.ID))
	}

	opts := SenderOptionsFromMap(spec.Options)
	return NewSender(mqttSession, opts), nil
}
