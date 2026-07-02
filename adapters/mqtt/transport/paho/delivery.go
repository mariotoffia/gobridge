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
//
// # Design decision: in-flight QoS 1/2 settlement on session step-down
//
// When an exclusive session loses its lease (or the process shuts down)
// this adapter does NOT gate consumption before disconnecting. The
// ratified model is forward-in-flight + broker-redelivery: Session.Close
// (session_lifecycle.go) flips the closed guard, disconnects the
// ConnectionManager, and awaits in-flight handlers bounded by the Close
// context. Because Route is synchronous and the Paho client sends the
// PUBACK/PUBCOMP only after Route returns (acl_router.go), a QoS 1/2
// publish that is mid-dispatch when the socket tears down is left
// UN-ACKED, so a persistent/exclusive broker session redelivers it to the
// next owner — at-least-once, with the durable outbox's version fence
// stopping a double-commit of the same record.
//
// Three options were considered; the first two were rejected:
//
//   - Drop-and-ACK (a stop-gate that stops pulling new work, then drains
//     the in-flight set): rejected. The Paho Router seam ACKs after Route
//     returns and cannot NACK, so "stop, then drain" would have to
//     drop-and-ACK whatever was already read — silently LOSING those
//     in-flight publishes. An earlier stop-gate along these lines was
//     reverted for exactly this reason.
//   - Disconnect-first-then-redeliver: the CHOSEN model above. Tearing the
//     socket down before acking is the only seam-safe way to force
//     redelivery, so it is preferred over any adapter-local drop.
//   - EnableManualAcknowledgment + post-emit ACK: the correct long-term
//     fix (ack only after emit has taken ownership, which enables a true
//     quiesce), but it is a Client-scoped re-architecture around
//     OnPublishReceived that is deliberately NOT taken here.
//
// True graceful drain — quiescing the source before disconnect so no
// in-flight publish is interrupted — is therefore deferred to the manager
// layer: the runtime SessionManager is the single owner of lease
// step-down and reconnect (finding C7) and is where a future
// EnableManualAcknowledgment path would land. Until then, step-down
// correctness rests on broker redelivery and the outbox fence, not on an
// adapter-local drain.
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
