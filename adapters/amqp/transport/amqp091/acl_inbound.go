package amqp091

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// deliveryToHeaders maps an amqp091.Delivery's system properties and
// user-defined headers to an envelope header map. Reserved x-bridge.*
// headers from the AMQP Headers table are stripped to prevent injection.
func deliveryToHeaders(d amqp.Delivery) map[string]any {
	h := make(map[string]any, 16+len(d.Headers))

	if d.MessageId != "" {
		h[HeaderMessageID] = d.MessageId
	}
	if d.CorrelationId != "" {
		h[HeaderCorrelationID] = d.CorrelationId
	}
	if d.ContentType != "" {
		h[HeaderContentType] = d.ContentType
	}
	if d.ContentEncoding != "" {
		h[HeaderContentEncoding] = d.ContentEncoding
	}
	if d.ReplyTo != "" {
		h[HeaderReplyTo] = d.ReplyTo
	}
	if d.Type != "" {
		h[HeaderType] = d.Type
	}
	if d.AppId != "" {
		h[HeaderAppID] = d.AppId
	}
	if d.DeliveryMode != 0 {
		h[HeaderDeliveryMode] = d.DeliveryMode
	}
	if d.Priority != 0 {
		h[HeaderPriority] = d.Priority
	}
	if d.Expiration != "" {
		h[HeaderExpiration] = d.Expiration
	}
	if !d.Timestamp.IsZero() {
		h[HeaderTimestamp] = d.Timestamp
	}
	if d.Exchange != "" {
		h[HeaderExchange] = d.Exchange
	}
	if d.RoutingKey != "" {
		h[HeaderRoutingKey] = d.RoutingKey
	}

	h[HeaderDeliveryTag] = d.DeliveryTag
	h[HeaderRedelivered] = d.Redelivered

	if d.ConsumerTag != "" {
		h[HeaderConsumerTag] = d.ConsumerTag
	}

	for k, v := range d.Headers {
		if k == HeaderGobridgeSubject {
			// Reserved cross-transport subject carrier — extracted
			// separately by deliveryToEnvelope into env.Subject(). Do
			// not duplicate it in the generic header pass-through.
			continue
		}
		if messaging.IsReservedHeader(k) || strings.HasPrefix(k, amqp091Prefix) {
			continue
		}
		// ACL purity: nested field tables/arrays and leaf SDK carriers
		// (amqp.Table, amqp.Decimal) must be rendered to stdlib types so no
		// SDK type crosses the boundary into envelope headers (PLUGIN.md
		// hard rule). Copying v verbatim leaked amqp.Table for nested tables.
		h[k] = renderAMQP091HeaderValue(v)
	}

	return h
}

// renderAMQP091HeaderValue converts an AMQP 0-9-1 field-table value into a
// domain-safe representation so no SDK type crosses the ACL into envelope
// headers. Nested field tables become map[string]any and field arrays
// become []any (both rendered recursively); amqp.Decimal becomes a float64;
// stdlib primitives (including []byte and time.Time) pass through unchanged.
// Any unexpected type is rendered via fmt.Sprint so a stray SDK carrier can
// never leak. Mirrors the amqp10 adapter's renderAMQP10AppPropertyValue.
func renderAMQP091HeaderValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case amqp.Table:
		return renderAMQP091Table(x)
	case map[string]any:
		return renderAMQP091Table(x)
	case []any:
		return renderAMQP091Array(x)
	case amqp.Decimal:
		return decimalToFloat(x)
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		[]byte, time.Time:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// renderAMQP091Table recursively renders every value of an AMQP field table
// into stdlib types, returning a plain map[string]any.
func renderAMQP091Table(t map[string]any) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = renderAMQP091HeaderValue(v)
	}
	return out
}

// renderAMQP091Array recursively renders every element of an AMQP field
// array into stdlib types, returning a plain []any.
func renderAMQP091Array(a []any) []any {
	out := make([]any, len(a))
	for i, v := range a {
		out[i] = renderAMQP091HeaderValue(v)
	}
	return out
}

// decimalToFloat converts an amqp.Decimal (mantissa Value scaled by 10^Scale)
// to the float64 it represents, e.g. {Scale:2, Value:12345} -> 123.45.
func decimalToFloat(d amqp.Decimal) float64 {
	return float64(d.Value) / math.Pow10(int(d.Scale))
}

// deliveryToEnvelope translates an inbound *amqp091.Delivery to a fresh
// messaging.Envelope. The CreatedAt field falls back to clk.Now() when the
// inbound message carries no timestamp. Returns an error when the
// validating constructor rejects the input (e.g. ID generation fails);
// callers MAY wrap into shared.ErrCodeInvalidPayload at the adapter
// boundary via wrapEnvelopeErr.
func deliveryToEnvelope(d amqp.Delivery, clk clock.Clock) (*messaging.Envelope, error) {
	if clk == nil {
		clk = clock.System
	}
	id := d.MessageId
	if id == "" {
		id = generateEnvelopeID()
	}
	created := clk.Now()
	if !d.Timestamp.IsZero() {
		created = d.Timestamp
	}
	var expiresAt time.Time
	if ttl, ok := parseExpirationMillis(d.Expiration); ok {
		// AMQP per-message expiration is a RELATIVE TTL (whole ms). Map it
		// to an ABSOLUTE deadline so multi-hop routing does not restart the
		// countdown at every bridge hop — egress re-derives the relative
		// TTL from ExpiresAt via Envelope.RemainingTTL. Anchored at receipt
		// time (not d.Timestamp), because the broker's TTL clock is what we
		// observe now; the remaining budget shrinks by the in-bridge dwell.
		expiresAt = clk.Now().Add(ttl)
	}
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        id,
		Subject:   subjectFromHeaders(d.Headers),
		Payload:   d.Body,
		Headers:   deliveryToHeaders(d),
		CreatedAt: created,
		ExpiresAt: expiresAt,
	}, clk.Now())
	if err != nil {
		return nil, wrapEnvelopeErr(err)
	}
	return env, nil
}

// parseExpirationMillis parses an AMQP 0-9-1 per-message expiration — a
// short string of whole milliseconds — into a duration. Empty, non-numeric,
// negative, or over-range (would overflow the duration) values yield
// ok=false so no expiry is mapped.
func parseExpirationMillis(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || ms < 0 {
		return 0, false
	}
	// time.Duration is int64 NANOSECONDS, so ms*time.Millisecond overflows
	// (and wraps NEGATIVE) once ms exceeds MaxInt64/1e6 (~9.22e12 ms, ~292
	// years). A wrapped-negative TTL would make ExpiresAt land in the PAST,
	// so the delivery is dropped/DLQ'd as already-expired — silent loss for
	// a producer who set a huge "effectively never expire" sentinel TTL.
	// Fail toward delivery: an over-range TTL maps to NO expiry rather than
	// a past deadline.
	if ms > int64(math.MaxInt64)/int64(time.Millisecond) {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// subjectFromHeaders extracts the logical Envelope.Subject from an
// inbound AMQP Headers table. The subject is carried by a typed string
// entry under HeaderGobridgeSubject; if absent or not a string, the
// returned subject is empty (the AMQP routing key is NEVER promoted to
// Envelope.Subject — that coupling was intentionally removed).
func subjectFromHeaders(table amqp.Table) string {
	if table == nil {
		return ""
	}
	if v, ok := table[HeaderGobridgeSubject].(string); ok {
		return v
	}
	return ""
}

func generateConsumerTag() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return "gobridge-" + hex.EncodeToString(b)
}
