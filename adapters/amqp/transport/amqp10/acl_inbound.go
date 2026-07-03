package amqp10

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		if msg.Properties.MessageID != nil {
			h[headerMessageID] = msg.Properties.MessageID
		}
		if msg.Properties.CorrelationID != nil {
			h[headerCorrelationID] = msg.Properties.CorrelationID
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
		h[k] = v
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

// receiverLink wraps a *amqp.Receiver, exposing only the operations
// the adapter's Receiver needs and converting incoming messages to
// domain-typed *Delivery values inside the ACL.
type receiverLink struct {
	raw *amqp.Receiver
	// delayUnhonoredWarn dedupes the delayed-retry-unhonored Warn to once
	// per link (G-N2). It is shared with every Delivery this link creates.
	delayUnhonoredWarn sync.Once
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
		// Reject malformed message at the broker so it is not redelivered
		// in an infinite loop. The errIngressRejected marker tells the
		// receive loop this is a handled per-message event (settled via
		// Reject), NOT a link fault — the loop counts it and continues.
		_ = r.raw.RejectMessage(ctx, msg, nil)
		return nil, fmt.Errorf("%w: %w", errIngressRejected, err)
	}
	d := NewDelivery(env, msg, r.raw, logger, metrics, clk)
	// Share the per-link warn guard so an unhonored delayed retry warns
	// once per link, not once per message (G-N2).
	d.delayWarnOnce = &r.delayUnhonoredWarn
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
		if s, ok := msg.Properties.MessageID.(string); ok {
			msgID = s
		}
	}
	if msgID == "" {
		msgID = generateEnvelopeID()
	}

	var subject string
	if msg.Properties != nil && msg.Properties.Subject != nil {
		subject = *msg.Properties.Subject
	}

	var body []byte
	if len(msg.Data) > 0 {
		body = msg.Data[0]
	} else if msg.Value != nil {
		if b, ok := msg.Value.([]byte); ok {
			body = b
		}
	}

	// A received broker absolute-expiry is stamped at construction (permissive):
	// a message that expired in transit (its AbsoluteExpiryTime is already past
	// at receive time) is accepted as an already-expired envelope and dropped
	// downstream by the TTL/IsExpired logic — it must NOT fail ingress, which
	// on this transport would tear down the receiver link.
	var expiresAt time.Time
	if msg.Properties != nil && msg.Properties.AbsoluteExpiryTime != nil {
		expiresAt = *msg.Properties.AbsoluteExpiryTime
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

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
