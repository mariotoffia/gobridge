package amqp091

import (
	"context"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// headersToPublishing maps envelope headers back to an amqp091.Publishing.
// Well-known amqp091.* headers are extracted into typed AMQP properties;
// remaining headers (excluding amqp091.* prefixed and reserved x-bridge.*)
// are placed in the AMQP Headers table.
func headersToPublishing(headers map[string]any) amqp.Publishing {
	pub := amqp.Publishing{}
	if headers == nil {
		return pub
	}

	if v, ok := headers[HeaderMessageID].(string); ok {
		pub.MessageId = v
	}
	if v, ok := headers[HeaderCorrelationID].(string); ok {
		pub.CorrelationId = v
	}
	if v, ok := headers[HeaderContentType].(string); ok {
		pub.ContentType = v
	}
	if v, ok := headers[HeaderContentEncoding].(string); ok {
		pub.ContentEncoding = v
	}
	if v, ok := headers[HeaderReplyTo].(string); ok {
		pub.ReplyTo = v
	}
	if v, ok := headers[HeaderType].(string); ok {
		pub.Type = v
	}
	if v, ok := headers[HeaderAppID].(string); ok {
		pub.AppId = v
	}
	if v, ok := headers[HeaderDeliveryMode].(uint8); ok {
		pub.DeliveryMode = v
	}
	if v, ok := headers[HeaderPriority].(uint8); ok {
		pub.Priority = v
	}
	if v, ok := headers[HeaderExpiration].(string); ok {
		pub.Expiration = v
	}
	if v, ok := headers[HeaderTimestamp].(time.Time); ok {
		pub.Timestamp = v
	}

	var table amqp.Table
	for k, v := range headers {
		if amqp091WellKnown[k] || strings.HasPrefix(k, amqp091Prefix) {
			continue
		}
		if messaging.IsInternalOnlyHeader(k) {
			// Strip internal-only dispatch bookkeeping (route-id,
			// route-override, source-id, content-type) but PRESERVE
			// bridge-to-bridge headers (correlation/causation/idempotency,
			// ordering, tenant, forwarded-from/hop, trace) so a downstream
			// bridge hop can deduplicate, correlate, and break loops.
			continue
		}
		// HeaderGobridgeSubject is handled by envelopeToPublishing
		// (which has access to env.Subject()). Skip it here so a stale
		// header copy does not race the typed write below.
		if k == HeaderGobridgeSubject {
			continue
		}
		if table == nil {
			table = make(amqp.Table, len(headers))
		}
		table[k] = v
	}
	pub.Headers = table

	return pub
}

// envelopeToPublishing builds an amqp091.Publishing from a messaging.Envelope.
// It maps the envelope body, ID, subject, TTL, and headers.
//
// The logical Envelope.Subject is propagated as a HeaderGobridgeSubject
// entry in the AMQP Headers table — distinct from the transport-level
// routing key chosen by the Sender. When env.Subject() is empty but a
// peer bridge supplied a HeaderGobridgeSubject in env.Headers() (subject
// round-trip from another transport), that value is preserved.
func envelopeToPublishing(env *messaging.Envelope, cfg SenderConfig, clk clock.Clock) amqp.Publishing {
	if clk == nil {
		clk = clock.System
	}
	pub := headersToPublishing(env.Headers())
	pub.Body = env.Payload()

	if env.ID() != "" && pub.MessageId == "" {
		pub.MessageId = env.ID()
	}

	switch {
	case env.Subject() != "":
		if pub.Headers == nil {
			pub.Headers = amqp.Table{}
		}
		pub.Headers[HeaderGobridgeSubject] = env.Subject()
	case env.Headers() != nil:
		if v, ok := env.Headers()[HeaderGobridgeSubject].(string); ok && v != "" {
			if pub.Headers == nil {
				pub.Headers = amqp.Table{}
			}
			pub.Headers[HeaderGobridgeSubject] = v
		}
	}

	if env.HasExpiry() {
		if ttl := env.RemainingTTL(clk); ttl > 0 {
			pub.Expiration = fmt.Sprintf("%d", ttl.Milliseconds())
		}
	}

	if pub.ContentType == "" && env.Headers() != nil {
		if ct, ok := env.Headers()[messaging.HeaderContentType].(string); ok {
			pub.ContentType = ct
		}
	}

	return pub
}

// unroutableError is returned by senderChannel.PublishConfirmed when the
// broker emits a basic.return for a Mandatory publish (i.e. the message
// matched no queue). It is mapped to shared.ErrNotFound at the seam.
type unroutableError struct {
	ReplyCode  uint16
	ReplyText  string
	Exchange   string
	RoutingKey string
}

func (e *unroutableError) Error() string {
	return "amqp091: mandatory publish unroutable: " + e.ReplyText
}

// senderChannel wraps the SDK channel + publisher-confirm bookkeeping for
// the Sender. All SDK access — Confirm select-mode, NotifyPublish,
// NotifyReturn, PublishWithContext — is encapsulated here so that the
// Sender stays SDK-free.
//
// The wrapper deliberately preserves the original ordering guarantees
// of basic.return-before-basic.ack documented on Sender.checkReturned:
// it does NOT spawn forwarding goroutines; instead PublishConfirmed
// reads the SDK confirm/return channels directly under the caller's
// mutex, the same way the previous implementation did inline.
//
// Concurrency: PublishConfirmed is NOT safe for concurrent use. The
// Sender serialises publish+confirm under its own mutex.
type senderChannel struct {
	ch        *amqpChannel
	confirms  chan amqp.Confirmation
	returns   chan amqp.Return
	mandatory bool
}

// openSenderChannel opens a fresh AMQP channel on the connection and
// installs publisher-confirm bookkeeping.
func openSenderChannel(conn amqpConnection, mandatory bool) (*senderChannel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.raw.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("amqp091: enable confirms: %w", err)
	}
	sc := &senderChannel{
		ch:        ch,
		confirms:  ch.raw.NotifyPublish(make(chan amqp.Confirmation, 1)),
		mandatory: mandatory,
	}
	if mandatory {
		sc.returns = ch.raw.NotifyReturn(make(chan amqp.Return, 1))
	}
	return sc, nil
}

// Close closes the underlying AMQP channel.
func (sc *senderChannel) Close() error { return sc.ch.Close() }

// drainReturns is a defensive no-blocking drain of any residual
// basic.return frames left on the returns chan. See the rationale on
// the publish path in Sender.Send.
func (sc *senderChannel) drainReturns() {
	if sc.returns == nil {
		return
	}
	for {
		select {
		case <-sc.returns:
			continue
		default:
			return
		}
	}
}

// publishResult captures the outcome of PublishConfirmed in a form the
// SDK-free Sender can act on without touching SDK types.
type publishResult struct {
	// PublishOK is true once the server has acked the publish.
	PublishOK bool
	// ConfirmedTag is the broker's delivery tag for the confirmation.
	ConfirmedTag uint64
	// Returned, when non-nil, indicates the broker bounced a Mandatory
	// publish as unroutable. The caller maps this to ErrNotFound.
	Returned *unroutableError
}

// PublishConfirmed publishes the envelope and waits for the publisher
// confirm on this channel. The returned publishResult lets the caller
// distinguish ack/nack/unroutable without ever observing SDK types.
//
// On any transport-level error (publish failure, ctx done, confirm
// channel closed) the channel is unusable and must be discarded by the
// caller (Sender.resetChannelLocked closes it).
func (sc *senderChannel) PublishConfirmed(
	ctx context.Context,
	exchange, routingKey string,
	mandatory bool,
	env *messaging.Envelope,
	cfg SenderConfig,
	clk clock.Clock,
) (publishResult, error) {
	pub := envelopeToPublishing(env, cfg, clk)

	// Defensive drain: under the deterministic ordering documented on
	// Sender.checkReturned every prior Send fully consumes its own
	// basic.return, so this loop should never find anything. If it
	// does, it indicates a code-path that bypassed the inspection
	// (e.g., a publish path with Mandatory toggled at runtime); drop
	// any such residue rather than mis-attribute it to the next Send.
	sc.drainReturns()

	if err := sc.ch.publishContext(ctx, exchange, routingKey, mandatory, pub); err != nil {
		return publishResult{}, err
	}

	select {
	case <-ctx.Done():
		return publishResult{}, ctx.Err()
	case confirmation, ok := <-sc.confirms:
		if !ok {
			return publishResult{}, errConfirmChannelClosed
		}
		if !confirmation.Ack {
			return publishResult{}, errPublishNacked
		}
		res := publishResult{PublishOK: true, ConfirmedTag: confirmation.DeliveryTag}
		if mandatory && sc.returns != nil {
			if rerr := checkReturn(sc.returns); rerr != nil {
				res.Returned = rerr
			}
		}
		return res, nil
	}
}

// checkReturn performs a non-blocking poll for a basic.return frame
// the broker emits when a Mandatory publish is unroutable. It MUST be
// non-blocking: under the deterministic ordering documented on
// (*senderChannel).PublishConfirmed, by the time a confirm has been
// observed the matching return is already buffered.
func checkReturn(returnsCh chan amqp.Return) *unroutableError {
	if returnsCh == nil {
		return nil
	}
	select {
	case ret, ok := <-returnsCh:
		if !ok {
			return nil
		}
		return &unroutableError{
			ReplyCode:  ret.ReplyCode,
			ReplyText:  ret.ReplyText,
			Exchange:   ret.Exchange,
			RoutingKey: ret.RoutingKey,
		}
	default:
		return nil
	}
}

// errConfirmChannelClosed and errPublishNacked are sentinel values used
// by the senderChannel/Sender pair to communicate confirmation outcomes
// without leaking SDK error types.
var (
	errConfirmChannelClosed = sentinelError("amqp091: confirm channel closed")
	errPublishNacked        = sentinelError("amqp091: publish nacked by broker")
)

type sentinelError string

func (e sentinelError) Error() string { return string(e) }
