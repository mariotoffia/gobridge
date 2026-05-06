// Delivery contract notes (file-level).
//
// MQTT QoS delivery guarantees are owned by the autopaho client library,
// not by the application layer. This causes deliberate asymmetries in the
// ports.Delivery implementation when compared to SQS
// (see adapters/aws/transport/sqs/delivery.go), where Ack/Retry/Extend
// map to real broker-side operations (DeleteMessage,
// ChangeMessageVisibility, redrive):
//
//   - Ack is a no-op: the PUBACK (QoS 1) or PUBREC/PUBCOMP (QoS 2) was
//     already sent by autopaho before the inbound handler ever ran. There
//     is no application-layer handle to acknowledge.
//   - Retry returns shared.ErrNotSupported: MQTT has no broker-side
//     visibility timeout or per-message redelivery primitive akin to SQS.
//   - Extend returns shared.ErrNotSupported for the same reason — there
//     is nothing to extend.
//
// Routes that need at-least-once semantics on top of an MQTT source must
// use DeliveryMode shared_outbox; durability comes from the outbox store
// and the egress sender, NOT from broker redelivery.
package paho

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	return shared.ErrNotSupported
}

func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
