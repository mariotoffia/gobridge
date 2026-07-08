package amqp10

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// headersToMessage maps envelope headers to AMQP 1.0 message properties
// and application properties.
func headersToMessage(headers map[string]any) *amqp.Message {
	msg := &amqp.Message{}
	props := &amqp.MessageProperties{}
	hasProps := false

	if v, ok := headers[headerMessageID]; ok {
		props.MessageID = v
		hasProps = true
	}
	if v, ok := headers[headerCorrelationID]; ok {
		props.CorrelationID = v
		hasProps = true
	}
	if v, ok := headers[headerContentType]; ok {
		if s, ok := v.(string); ok {
			props.ContentType = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerContentEncoding]; ok {
		if s, ok := v.(string); ok {
			props.ContentEncoding = &s
			hasProps = true
		}
	}
	// Finding 5 (domain invariant): the amqp10.subject header is
	// deliberately NOT mapped to Properties.Subject. Envelope.Subject is
	// the SOLE egress source for the AMQP Subject (applied in
	// envelopeToMessage), so a free-form amqp10.subject header can never
	// override or spoof it when Envelope.Subject is empty.
	if v, ok := headers[headerTo]; ok {
		if s, ok := v.(string); ok {
			props.To = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerReplyTo]; ok {
		if s, ok := v.(string); ok {
			props.ReplyTo = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupID]; ok {
		if s, ok := v.(string); ok {
			props.GroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupSequence]; ok {
		// F12 / FIX 5: group-sequence is a uint32 on the wire. JSON headers
		// arrive as float64 and cross-bridge headers may be int/int64/uint;
		// accept the common numeric carriers, bounded to [0, MaxUint32], and
		// drop an out-of-range or non-integral value (leaving it unset)
		// rather than silently wrapping to a small, wrong sequence number.
		if seq, ok := amqp10GroupSequence(v); ok {
			props.GroupSequence = &seq
			hasProps = true
		}
	}
	if v, ok := headers[headerReplyToGroupID]; ok {
		if s, ok := v.(string); ok {
			props.ReplyToGroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerCreationTime]; ok {
		if t, ok := v.(time.Time); ok {
			props.CreationTime = &t
			hasProps = true
		}
	}
	if v, ok := headers[headerAbsoluteExpiry]; ok {
		if t, ok := v.(time.Time); ok {
			props.AbsoluteExpiryTime = &t
			hasProps = true
		}
	}

	if hasProps {
		msg.Properties = props
	}

	// Central egress header policy: strip INTERNAL-ONLY reserved headers
	// (route-id, route-override, source-id, content-type) so bridge
	// dispatch bookkeeping never leaks to a consumer, while BRIDGE-TO-
	// BRIDGE PROPAGATED headers (correlation-id, idempotency-key,
	// ordering-key, tenant-id, traceparent, ...) pass through so a
	// receiving bridge can correlate, deduplicate and continue a trace.
	//
	// We strip via (IsReservedHeader && !IsBridgeToBridgeHeader) rather
	// than the literal StripInternalOnlyHeaders so that UNCLASSIFIED
	// x-bridge.* keys (anything reserved-prefixed that is neither
	// internal-only nor part of the bridge-to-bridge contract) are also
	// stripped — they are not a propagation contract and must not leak.
	// ponytail: a per-sender "bridge-to-bridge mode" toggle (strip ALL
	// reserved for an external-only sink) lives in shared config; absent
	// that, this is the safe default.
	var appProps map[string]any
	for k, v := range headers {
		if wellKnownHeaders[k] || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		if messaging.IsReservedHeader(k) && !messaging.IsBridgeToBridgeHeader(k) {
			continue
		}
		if appProps == nil {
			appProps = make(map[string]any, len(headers))
		}
		appProps[k] = v
	}
	msg.ApplicationProperties = appProps

	return msg
}

// amqp10GroupSequence normalises a group-sequence header value to the
// uint32 the AMQP 1.0 wire uses (F12 / FIX 5). It accepts the numeric
// carriers a header can arrive as — uint32, int, int64, uint, uint64 and
// float64 (JSON decodes integers as float64) — and reports ok only when
// the value is a non-negative integer within [0, MaxUint32]. A fractional
// or out-of-range value returns ok=false so the caller leaves the sequence
// unset instead of silently wrapping or truncating.
func amqp10GroupSequence(v any) (uint32, bool) {
	switch n := v.(type) {
	case uint32:
		return n, true
	case int:
		if n >= 0 && uint64(n) <= math.MaxUint32 {
			return uint32(n), true
		}
	case int64:
		if n >= 0 && uint64(n) <= math.MaxUint32 {
			return uint32(n), true
		}
	case uint:
		if uint64(n) <= math.MaxUint32 {
			return uint32(n), true
		}
	case uint64:
		if n <= math.MaxUint32 {
			return uint32(n), true
		}
	case float64:
		if n >= 0 && n <= math.MaxUint32 && math.Trunc(n) == n {
			return uint32(n), true
		}
	}
	return 0, false
}

// envelopeToMessage builds an outbound *amqp.Message from an envelope,
// merging headers, payload, and any envelope-level fields (ID, subject,
// expiry, creation time) into a single SDK message.
//
// durable stamps the AMQP message-header durable flag. Brokers treat
// durable=false as non-persistent: the message is lost on broker
// restart even after an accepted disposition, silently breaking the
// bridge's at-least-once contract. Senders default this to true (see
// SenderConfig.Durable).
func envelopeToMessage(env *messaging.Envelope, durable bool) *amqp.Message {
	msg := headersToMessage(env.Headers())
	msg.Data = [][]byte{env.Payload()}

	if durable {
		msg.Header = &amqp.MessageHeader{Durable: true}
	}

	if msg.Properties == nil {
		msg.Properties = &amqp.MessageProperties{}
	}
	// headerMessageID, when present, carries the DETERMINISTIC STRING
	// rendering of the inbound message-id: messageToHeaders renders a
	// typed uuid/ulong/binary id via messageIDToString so no go-amqp SDK
	// type ever reaches the domain envelope headers (ACL purity,
	// finding F9). headersToMessage therefore set Properties.MessageID to
	// that string, and egress emits a string message-id. Downstream
	// message-id dedup still holds because the rendering is stable
	// (same id → same string). Only stamp the (string) envelope ID when
	// no message-id survived the hop.
	if msg.Properties.MessageID == nil && env.ID() != "" {
		msg.Properties.MessageID = env.ID()
	}
	if s := env.Subject(); s != "" {
		msg.Properties.Subject = &s
	}
	if env.HasExpiry() {
		expiry := env.ExpiresAt()
		msg.Properties.AbsoluteExpiryTime = &expiry
	}
	// Preserve the PRODUCER's creation-time when the inbound hop carried
	// one (mapped from the amqp10.creation-time header by
	// headersToMessage). Only fall back to Envelope.CreatedAt — which on
	// a relayed message is the bridge RECEIVE time — when no producer
	// timestamp is present, so egress never clobbers the original.
	if msg.Properties.CreationTime == nil && !env.CreatedAt().IsZero() {
		createdAt := env.CreatedAt()
		msg.Properties.CreationTime = &createdAt
	}
	return msg
}

// errNotAccepted marks a send whose disposition outcome was anything
// other than Accepted (Released, Modified, Rejected, or unknown). The
// LINK is healthy in this case — the broker answered — so Sender.Send
// must NOT detach/rebuild the link for these errors.
var errNotAccepted = errors.New("amqp10: broker did not accept message")

// dispositionError classifies a send receipt's terminal delivery state.
//
//   - Accepted           → nil (enqueued durably per the message header)
//   - Released/Modified  → transient ErrUnavailable: the broker could
//     not or did not enqueue the message; treating this as success (as
//     plain Send does — it only fails on Rejected) would Ack the source
//     and silently lose the message.
//   - Rejected           → permanent by default (broker deems the
//     message unprocessable); a broker-supplied error condition is
//     mapped through the normal condition table.
func dispositionError(state amqp.DeliveryState) error {
	switch st := state.(type) {
	case *amqp.StateAccepted:
		return nil
	case *amqp.StateRejected:
		if st.Error != nil {
			return mapAMQPCondition(st.Error).Wrap(fmt.Errorf("%w: rejected: %w", errNotAccepted, st.Error))
		}
		return shared.ErrInvalidPayload.Wrap(errNotAccepted).
			WithMessage("amqp10: message rejected by broker without error condition")
	case *amqp.StateReleased:
		return shared.ErrUnavailable.Wrap(errNotAccepted).
			WithMessage("amqp10: message released by broker (not enqueued)")
	case *amqp.StateModified:
		return shared.ErrUnavailable.Wrap(errNotAccepted).
			WithMessage("amqp10: message returned modified by broker (not enqueued)")
	default:
		return shared.ErrUnavailable.Wrap(errNotAccepted).
			WithMessage(fmt.Sprintf("amqp10: unexpected delivery state %T", state))
	}
}

// senderLink wraps a *amqp.Sender, exposing only an envelope-typed
// Send. *amqp.Sender.SendWithReceipt is documented as safe for
// concurrent use, so this wrapper preserves that contract: SendEnvelope
// may be invoked from many goroutines at once.
type senderLink struct {
	raw *amqp.Sender
	// durable stamps the message-header durable flag on every message
	// sent over this link (captured from SenderConfig at link creation).
	durable bool
}

// SendEnvelope serialises the envelope into an AMQP 1.0 message and
// publishes it over the link, waiting for the broker's disposition.
// Unlike raw Send (which reports success for Released/Modified), any
// non-Accepted outcome is surfaced as an error so the caller never
// Acks the source for a message the broker did not enqueue.
func (s *senderLink) SendEnvelope(ctx context.Context, env *messaging.Envelope) error {
	msg := envelopeToMessage(env, s.durable)
	receipt, err := s.raw.SendWithReceipt(ctx, msg, nil)
	if err != nil {
		return fmt.Errorf("amqp10: send: %w", err)
	}
	state, err := receipt.Wait(ctx)
	if err != nil {
		return fmt.Errorf("amqp10: send settlement: %w", err)
	}
	return dispositionError(state)
}

// Close closes the link with the supplied context as detach timeout.
func (s *senderLink) Close(ctx context.Context) error {
	if s == nil || s.raw == nil {
		return nil
	}
	if err := s.raw.Close(ctx); err != nil {
		return fmt.Errorf("amqp10: sender close: %w", err)
	}
	return nil
}
