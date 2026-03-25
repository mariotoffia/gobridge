package paho

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an incoming MQTT message.
// MQTT protocol-level acknowledgement (PUBACK for QoS 1, PUBREC/PUBCOMP for
// QoS 2) is handled internally by the Paho client, so Ack is a no-op.
// Retry and Extend are not supported by MQTT.
type Delivery struct {
	env *domain.Envelope
}

// NewDelivery wraps an Envelope as a ports.Delivery.
func NewDelivery(env *domain.Envelope) *Delivery {
	return &Delivery{env: env}
}

func (d *Delivery) Envelope() *domain.Envelope { return d.env }

func (d *Delivery) Ack(_ context.Context) error { return nil }

func (d *Delivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return domain.ErrNotSupported
}

func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return domain.ErrNotSupported
}
