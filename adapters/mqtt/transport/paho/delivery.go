// Delivery contract notes (file-level).
//
// This adapter runs the Paho client with EnableManualAcknowledgment:
// the protocol acknowledgement (PUBACK for QoS 1, PUBREC/PUBCOMP for
// QoS 2) is NOT sent when the message is read off the wire, and NOT
// sent when the router's dispatch returns — it is sent only when the
// runtime SETTLES the delivery by calling Delivery.Ack. For a
// shared_outbox route that is after the outbox Persist completes; for
// a direct_hold route it is after the target accepted. A crash at any
// point before settlement leaves the QoS 1/2 message un-acked on the
// broker, and a persistent/exclusive broker session redelivers it to
// the next owner — true ack-after-durable-handoff.
//
// Ordering: the MQTT spec requires QoS 1/2 acknowledgements to be sent
// in the order the PUBLISH packets were received. The Paho client
// handles this internally (paho.Client acksTracker): Delivery.Ack marks
// the packet acknowledged, and the client flushes acknowledgements to
// the broker strictly in receive order — out-of-order settlement by
// concurrent route runners is therefore safe, at the cost of
// head-of-line blocking behind the oldest unsettled delivery.
//
// Residual boundaries:
//
//   - There is no per-message NACK in MQTT. A delivery that is never
//     settled (emit error, pipeline crash) simply stays un-acked; the
//     broker redelivers it on the next session resume. Durability for
//     the in-connection error path must come from the outbox/DLQ.
//
//   - QoS 0 and ephemeral (clean-start) sessions have no broker
//     redelivery at all — delivery is best-effort by protocol, and
//     Ack is a no-op for QoS 0.
//
//   - An Ack issued after the underlying connection was torn down and
//     re-established cannot be delivered (the client's ack tracker was
//     reset); it reports success because the broker will redeliver and
//     downstream idempotency/dedup absorbs the duplicate.
//
//   - Retry returns shared.ErrNotSupported: MQTT has no broker-side
//     visibility timeout or per-message redelivery primitive akin to
//     SQS. The route runner falls back to DLQ routing.
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
// # Session step-down / shutdown
//
// Session.Close disconnects without draining: any delivery that is
// unsettled when the socket tears down is left un-acked, so a
// persistent/exclusive broker session redelivers it to the next owner —
// at-least-once, with the durable outbox's version fence stopping a
// double-commit of the same record.
package paho

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an incoming MQTT message.
// Ack triggers the MQTT protocol acknowledgement (PUBACK / PUBREC-
// PUBCOMP via the Paho manual-acknowledgment API); Retry and Extend
// are not supported by MQTT.
type Delivery struct {
	env *messaging.Envelope
	ack func() error

	ackOnce sync.Once
	ackErr  error
}

// DeliveryOption customises a Delivery.
type DeliveryOption func(*Delivery)

// WithAckFunc installs the protocol-acknowledgement callback invoked by
// the first Ack call. The router wires this to the Paho client's manual
// Ack for the originating publish.
func WithAckFunc(fn func() error) DeliveryOption {
	return func(d *Delivery) { d.ack = fn }
}

// NewDelivery wraps an Envelope as a ports.Delivery.
func NewDelivery(env *messaging.Envelope, opts ...DeliveryOption) *Delivery {
	d := &Delivery{env: env}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (d *Delivery) Envelope() *messaging.Envelope { return d.env }

// Ack sends the MQTT protocol acknowledgement for the originating
// publish (deferred-until-settlement; see the file-level contract
// notes). Ack is idempotent: the protocol acknowledgement fires at most
// once and subsequent calls return the first result. A Delivery
// constructed without an ack callback (QoS 0, tests, legacy Route path)
// acks as a no-op.
func (d *Delivery) Ack(_ context.Context) error {
	if d.ack == nil {
		return nil
	}
	d.ackOnce.Do(func() { d.ackErr = d.ack() })
	return d.ackErr
}

func (d *Delivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return shared.ErrNotSupported
}

func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
