// Delivery contract notes (file-level).
//
// MQTT QoS delivery guarantees are owned by the autopaho client library,
// not by the application layer. This causes deliberate asymmetries in the
// ports.Delivery implementation when compared to SQS
// (see adapters/aws/transport/sqs/delivery.go), where Ack/Retry/Extend
// map to real broker-side operations (DeleteMessage,
// ChangeMessageVisibility, redrive):
//
//   - Ack is a no-op: the PUBACK (QoS 1) or PUBREC/PUBCOMP (QoS 2) is sent
//     by the Paho client's routePublishPackets goroutine when the router's
//     Route call returns — there is no application-layer handle to send it.
//     The adapter narrows the loss window by dispatching SYNCHRONOUSLY:
//     Route blocks until emit returns (see acl_router.go), so the
//     acknowledgement is deferred until AFTER the handler has taken
//     ownership (for shared_outbox, until the outbox Persist completes).
//     A crash strictly BEFORE Route returns therefore leaves the QoS 1/2
//     message un-acked, and a persistent broker session redelivers it.
//
//     Residual boundary (cannot be closed with the Paho Router seam):
//
//   - On emit ERROR the message has still been read by Paho and will
//     be auto-acked once Route unwinds; there is no per-message NACK.
//     Durability for the error path must come from the outbox/DLQ, not
//     from broker redelivery.
//
//   - QoS 0 and ephemeral (clean-start) sessions have no broker
//     redelivery at all — delivery is best-effort by protocol.
//     True application-controlled settlement would require Paho's
//     EnableManualAcknowledgment + OnPublishReceived (Client-scoped ack),
//     a larger re-architecture deliberately not taken here.
//
//   - Retry returns shared.ErrNotSupported: MQTT has no broker-side
//     visibility timeout or per-message redelivery primitive akin to SQS.
//
//   - Extend returns shared.ErrNotSupported for the same reason — there
//     is nothing to extend.
//
// QoS/retain are broker<->client PACKET semantics only, not end-to-end
// application guarantees: QoS 1/2 governs the hop between this client and
// its broker, and retain is broker-side last-value storage. They do NOT
// imply the egress sender, a downstream broker, or the ultimate consumer
// received the message.
//
// Routes that need at-least-once semantics on top of an MQTT source must
// use DeliveryMode shared_outbox; durability comes from the outbox store
// and the egress sender, NOT from broker redelivery.
package paho

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an incoming MQTT message.
// MQTT protocol-level acknowledgement (PUBACK for QoS 1, PUBREC/PUBCOMP for
// QoS 2) is handled internally by the Paho client, so Ack is a no-op.
// Retry and Extend are not supported by MQTT.
type Delivery struct {
	env *messaging.Envelope
}

// NewDelivery wraps an Envelope as a ports.Delivery.
func NewDelivery(env *messaging.Envelope) *Delivery {
	return &Delivery{env: env}
}

func (d *Delivery) Envelope() *messaging.Envelope { return d.env }

// Ack is a no-op. The MQTT protocol acknowledgement is sent by the Paho
// client when the router's synchronous Route call returns, not by the
// application — see the file-level contract notes for the deferred-ack
// behaviour and its residual at-least-once boundary.
func (d *Delivery) Ack(_ context.Context) error { return nil }

func (d *Delivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return shared.ErrNotSupported
}

func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
