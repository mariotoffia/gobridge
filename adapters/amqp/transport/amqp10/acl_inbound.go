package amqp10

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// messageToHeaders maps AMQP 1.0 message properties and application
// properties to envelope headers. Reserved bridge headers and AMQP
// 1.0 well-known headers are filtered.
func messageToHeaders(msg *amqp.Message) map[string]any {
	size := len(msg.ApplicationProperties) + 13
	h := make(map[string]any, size)

	if msg.Properties != nil {
		// render the message-id / correlation-id to their canonical
		// STRING form (messageIDToString handles string/uuid/ulong/binary)
		// so a go-amqp SDK type (amqp.UUID, []byte) never lands in the
		// domain envelope headers, where audit JSON would otherwise emit
		// raw byte arrays (ACL purity). An unrecognised (non-spec) id type
		// renders empty and is dropped rather than leaked.
		if id := messageIDToString(msg.Properties.MessageID); id != "" {
			h[headerMessageID] = id
		}
		if id := messageIDToString(msg.Properties.CorrelationID); id != "" {
			h[headerCorrelationID] = id
		}
		if msg.Properties.ContentType != nil {
			h[headerContentType] = *msg.Properties.ContentType
		}
		if msg.Properties.ContentEncoding != nil {
			h[headerContentEncoding] = *msg.Properties.ContentEncoding
		}
		if msg.Properties.Subject != nil {
			h[headerSubject] = *msg.Properties.Subject
		}
		if msg.Properties.To != nil {
			h[headerTo] = *msg.Properties.To
		}
		if msg.Properties.ReplyTo != nil {
			h[headerReplyTo] = *msg.Properties.ReplyTo
		}
		if msg.Properties.GroupID != nil {
			h[headerGroupID] = *msg.Properties.GroupID
		}
		if msg.Properties.GroupSequence != nil {
			h[headerGroupSequence] = *msg.Properties.GroupSequence
		}
		if msg.Properties.ReplyToGroupID != nil {
			h[headerReplyToGroupID] = *msg.Properties.ReplyToGroupID
		}
		if msg.Properties.CreationTime != nil {
			h[headerCreationTime] = *msg.Properties.CreationTime
		}
		if msg.Properties.AbsoluteExpiryTime != nil {
			h[headerAbsoluteExpiry] = *msg.Properties.AbsoluteExpiryTime
		}
	}

	if msg.Header != nil {
		h[headerDeliveryCount] = msg.Header.DeliveryCount
	}

	for k, v := range msg.ApplicationProperties {
		if messaging.IsReservedHeader(k) || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		h[k] = renderAMQP10AppPropertyValue(v)
	}

	return h
}

// linkReceiver is the subset of receiver-link operations the adapter's
// Receiver depends on. It enables test-double injection of the receive
// loop (mirroring the settler seam in acl_delivery.go) without leaking
// the SDK link type.
type linkReceiver interface {
	Receive(ctx context.Context, logger *slog.Logger, metrics ports.MetricsExporter, clk clock.Clock) (*Delivery, error)
	Close(ctx context.Context) error
}

// errIngressRejected marks an inbound message that failed envelope
// construction and was rejected at the broker. The receive LOOP must
// treat this as a handled, per-message event (count it and continue) —
// not as a link fault and NEVER as a terminal receiver error: one
// poison message must not tear down the route or the whole bridge.
var errIngressRejected = errors.New("amqp10: inbound message rejected at ingress")

// errUnrepresentableBody marks an inbound message whose body cannot be
// represented as a byte payload (a non-string/[]byte amqp-value, or an
// amqp-sequence-only body). messageToEnvelope returns it so the receive
// loop settles the message via the errIngressRejected path (counted +
// warned) instead of silently forwarding an EMPTY envelope while Acking
// and deleting the source — irrecoverable body loss (finding 1).
var errUnrepresentableBody = errors.New("amqp10: message body cannot be represented as bytes")

// receiverLink wraps a *amqp.Receiver, exposing only the operations
// the adapter's Receiver needs and converting incoming messages to
// domain-typed *Delivery values inside the ACL.
type receiverLink struct {
	raw rawReceiver
	// delayDeferredWarn dedupes the delayed-retry-deferred Warn to once
	// per link (G). It is shared with every Delivery this link creates.
	delayDeferredWarn sync.Once
}

// rawReceiver is the subset of *amqp.Receiver operations the receiverLink
// ACL wrapper depends on. It is defined inside the ACL boundary (where SDK
// types are permitted) so a test double can inject a receiver whose
// RejectMessage FAILS, making the malformed-ingress settlement-failure
// path unit-testable without a live broker. It embeds settler
// because a *Delivery settles through the same underlying link, so a
// *amqp.Receiver satisfies both in one value.
type rawReceiver interface {
	settler
	Receive(ctx context.Context, opts *amqp.ReceiveOptions) (*amqp.Message, error)
	RejectMessage(ctx context.Context, msg *amqp.Message, e *amqp.Error) error
	Address() string
	Close(ctx context.Context) error
}

var _ linkReceiver = (*receiverLink)(nil)

// Receive blocks until a message is available, then translates it into
// a fresh *Delivery (with envelope, settler, etc.) without leaking SDK
// types to the caller. The receiver does NOT fall back to the link
// address when Properties.Subject is missing; an inbound message
// without Properties.Subject yields an Envelope with empty Subject.
//
// MetricAMQP10ReceiveLatency measures only the post-arrival conversion
// work (message → envelope → delivery). The blocking wait for a message
// is deliberately EXCLUDED: timing the idle block would report minutes
// of "latency" on a quiet queue.
func (r *receiverLink) Receive(
	ctx context.Context,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) (*Delivery, error) {
	if clk == nil {
		clk = clock.System
	}
	msg, err := r.raw.Receive(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("amqp10: receive: %w", err)
	}
	arrived := clk.Now()
	env, err := messageToEnvelope(msg, clk)
	if err != nil {
		// A malformed message is rejected at the broker so it is not
		// redelivered in an infinite loop.: the reject is itself a
		// settlement that can FAIL (deadline exceeded, connection dropped);
		// if it does, the delivery is STILL UNSETTLED. Reporting
		// errIngressRejected here would emit a false "rejected" metric,
		// permanently leak this delivery's link-credit slot (go-amqp
		// replenishes credit only on a completed disposition), and let the
		// poison message redeliver in a loop while the link/session still
		// look healthy. Surface the settlement error instead (NOT
		// errIngressRejected) so the receive loop routes it through
		// handleLinkError, rebuilding the link/session and reissuing the
		// credit the broker holds for the unsettled delivery.
		if rejErr := r.raw.RejectMessage(ctx, msg, nil); rejErr != nil {
			return nil, fmt.Errorf("amqp10: reject malformed inbound message: %w", rejErr)
		}
		// The reject settled cleanly: mark it a handled per-message event
		// so the receive loop counts it and continues — one poison message
		// must not tear the route down.
		return nil, fmt.Errorf("%w: %w", errIngressRejected, err)
	}
	d := NewDelivery(env, msg, r.raw, logger, metrics, clk)
	// Share the per-link warn guard so a deferred delayed retry warns
	// once per link, not once per message (G).
	d.delayWarnOnce = &r.delayDeferredWarn
	if metrics != nil {
		metrics.Timer(MetricAMQP10ReceiveLatency, clk.Since(arrived),
			shared.Tag{Key: shared.TagKeyEntity, Value: r.raw.Address()})
	}
	return d, nil
}

// Close closes the receiver link. The supplied context bounds the
// detach timeout.
func (r *receiverLink) Close(ctx context.Context) error {
	if r == nil || r.raw == nil {
		return nil
	}
	if err := r.raw.Close(ctx); err != nil {
		return fmt.Errorf("amqp10: receiver close: %w", err)
	}
	return nil
}

// messageToEnvelope translates an inbound *amqp.Message into a fresh
// messaging.Envelope. The logical Envelope.Subject is sourced solely
// from Properties.Subject; if the inbound message has no Subject the
// envelope's Subject is left empty (no fallback to the link address).
// The raw amqp10.subject header is still recorded under
// envelope.Headers() via messageToHeaders for full property round-trip.
//
// Bridge-to-bridge identity headers: a peer bridge's egress deliberately
// propagates HeaderIdempotencyKey / HeaderDeduplicationID /
// HeaderOrderingKey as application properties (see headersToMessage).
// NewEnvelope strips every x-bridge.* key from untrusted Headers, so
// those values are LIFTED into EnvelopeInput's first-class fields — the
// trusted, anti-spoof-safe path — before the strip; otherwise dedup and
// ordering silently vanish at the receiving hop of a
// bridge→broker→bridge topology.
func messageToEnvelope(msg *amqp.Message, clk clock.Clock) (*messaging.Envelope, error) {
	if clk == nil {
		clk = clock.System
	}
	headers := messageToHeaders(msg)

	var msgID string
	if msg.Properties != nil && msg.Properties.MessageID != nil {
		msgID = messageIDToString(msg.Properties.MessageID)
	}
	if msgID == "" {
		msgID = generateEnvelopeID()
	}

	var subject string
	if msg.Properties != nil && msg.Properties.Subject != nil {
		subject = *msg.Properties.Subject
	}

	body, err := messageBody(msg)
	if err != nil {
		return nil, err
	}

	// A received broker absolute-expiry is stamped at construction (permissive):
	// a message that expired in transit (its AbsoluteExpiryTime is already past
	// at receive time) is accepted as an already-expired envelope and dropped
	// downstream by the TTL/IsExpired logic — it must NOT fail ingress, which
	// on this transport would tear down the receiver link.
	var expiresAt time.Time
	switch {
	case msg.Properties != nil && msg.Properties.AbsoluteExpiryTime != nil:
		expiresAt = *msg.Properties.AbsoluteExpiryTime
	case msg.Header != nil && msg.Header.TTL > 0:
		// a ttl-only message (Header.TTL set, no AbsoluteExpiryTime)
		// would otherwise cross the bridge as immortal — the relative
		// lifetime is dropped and nothing downstream can expire it. Stamp
		// a concrete expiry of receive-time + TTL so the producer's
		// intended lifetime is honored on this and subsequent hops.
		expiresAt = clk.Now().Add(msg.Header.TTL)
	}
	// Preserve the producer's creation-time when present; fall back to
	// the bridge receive time only for messages that carry none, so a
	// relayed envelope keeps its original timestamp across hops.
	createdAt := clk.Now()
	if msg.Properties != nil && msg.Properties.CreationTime != nil && !msg.Properties.CreationTime.IsZero() {
		createdAt = *msg.Properties.CreationTime
	}
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:              msgID,
		Subject:         subject,
		Payload:         body,
		Headers:         headers,
		CreatedAt:       createdAt,
		ExpiresAt:       expiresAt,
		IdempotencyKey:  bridgeHeaderString(msg, messaging.HeaderIdempotencyKey),
		DeduplicationID: bridgeHeaderString(msg, messaging.HeaderDeduplicationID),
		OrderingKey:     bridgeHeaderString(msg, messaging.HeaderOrderingKey),
	}, clk.Now())
	if err != nil {
		return nil, wrapEnvelopeErr(err)
	}
	return env, nil
}

// bridgeHeaderString reads a reserved bridge-to-bridge header from the
// message's application properties as a string (case-insensitive on the
// x-bridge. namespace, consistent with messaging.IsReservedHeader).
// Returns "" when absent or not a string.
func bridgeHeaderString(msg *amqp.Message, key string) string {
	for k, v := range msg.ApplicationProperties {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return s
			}
			return ""
		}
	}
	return ""
}

// messageBody extracts the byte payload from an inbound AMQP 1.0
// message. Body sections are handled per AMQP 1.0 §3.2:
//
//   - Data sections: the payload is the concatenation of ALL data
//     sections, not just the first — a multi-section body is the
//     logical concatenation of its parts (the previous code silently
//     truncated to Data[0]).
//   - amqp-value section: []byte is taken as-is; string is converted via
//     []byte(s) (what Qpid-JMS/Artemis TextMessage produces). Any OTHER
//     value type (map, list, number, null, amqp-sequence) cannot be
//     faithfully represented as bytes and is REJECTED via
//     errUnrepresentableBody so the message is settled through the
//     ingress-rejected path (counted + warned) rather than Acked-and-
//     forwarded empty (finding 1).
//   - No body sections at all: a legitimately empty message → nil body.
func messageBody(msg *amqp.Message) ([]byte, error) {
	switch {
	case len(msg.Data) == 1:
		return msg.Data[0], nil
	case len(msg.Data) > 1:
		total := 0
		for _, d := range msg.Data {
			total += len(d)
		}
		body := make([]byte, 0, total)
		for _, d := range msg.Data {
			body = append(body, d...)
		}
		return body, nil
	case msg.Value != nil:
		switch v := msg.Value.(type) {
		case []byte:
			return v, nil
		case string:
			return []byte(v), nil
		default:
			return nil, fmt.Errorf("%w: unrepresentable amqp-value body of type %T", errUnrepresentableBody, msg.Value)
		}
	case len(msg.Sequence) > 0:
		return nil, fmt.Errorf("%w: amqp-sequence body of %d section(s)", errUnrepresentableBody, len(msg.Sequence))
	default:
		return nil, nil
	}
}

// messageIDToString renders an AMQP 1.0 message-id (which may be a
// string, uuid, ulong, or binary in the AMQP 1.0 message-id type union)
// into the string form the
// envelope ID uses. A deterministic rendering (uuid canonical form,
// ulong decimal, binary hex) preserves downstream message-id dedup for
// non-string ids instead of substituting a random envelope ID
// (finding 6). Returns "" for an unrecognised type so the caller falls
// back to a generated ID.
func messageIDToString(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case amqp.UUID:
		return v.String()
	case uint64:
		return strconv.FormatUint(v, 10)
	case []byte:
		return hex.EncodeToString(v)
	default:
		return ""
	}
}

// renderAMQP10AppPropertyValue converts a go-amqp application-property
// value into a domain-safe representation so no SDK type crosses the ACL
// into envelope headers (the application-property path). Audit JSON would
// otherwise emit raw byte arrays or opaque amqp.* values. SDK carriers
// render deterministically (amqp.UUID -> canonical string, amqp.Symbol ->
// string, binary -> hex); stdlib primitives (including time.Time) pass
// through unchanged. Any other type is rendered via fmt.Sprint so a stray
// SDK type can never leak. Unlike messageIDToString this passes primitives
// through instead of erasing them to "".
func renderAMQP10AppPropertyValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case amqp.UUID:
		return x.String()
	case amqp.Symbol:
		return string(x)
	case []byte:
		return hex.EncodeToString(x)
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		time.Time:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
