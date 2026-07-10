package amqp091

import (
	"context"
	"fmt"
	"math"
	"strconv"
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
	if v, ok := deliveryModeFromHeader(headers[HeaderDeliveryMode]); ok {
		pub.DeliveryMode = v
	}
	if v, ok := priorityFromHeader(headers[HeaderPriority]); ok {
		pub.Priority = v
	}
	if v, ok := headers[HeaderExpiration].(string); ok {
		pub.Expiration = v
	}
	if v, ok := timestampFromHeader(headers[HeaderTimestamp]); ok {
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

// deliveryModeFromHeader coerces a per-message "amqp091.delivery-mode"
// header override into an AMQP delivery mode. Loop-back deliveries carry
// a typed uint8, but headers decoded from YAML/JSON route configs (or
// produced by another transport) arrive as int/int64/float64/string —
// rejecting those silently downgraded every override to the transient
// zero value. Accepted values are 1/2 (or "transient"/"persistent");
// anything else is ignored so the sender's configured default applies.
func deliveryModeFromHeader(v any) (uint8, bool) {
	toMode := func(n int64) (uint8, bool) {
		if n == int64(amqp.Transient) || n == int64(amqp.Persistent) {
			return uint8(n), true
		}
		return 0, false
	}
	switch m := v.(type) {
	case uint8:
		return toMode(int64(m))
	case int:
		return toMode(int64(m))
	case int8:
		return toMode(int64(m))
	case int16:
		return toMode(int64(m))
	case int32:
		return toMode(int64(m))
	case int64:
		return toMode(m)
	case uint16:
		return toMode(int64(m))
	case uint32:
		return toMode(int64(m))
	case uint64:
		if m > 2 {
			return 0, false
		}
		return toMode(int64(m))
	case float64:
		if m != float64(int64(m)) {
			return 0, false
		}
		return toMode(int64(m))
	case float32:
		return deliveryModeFromHeader(float64(m))
	case string:
		switch strings.ToLower(strings.TrimSpace(m)) {
		case DeliveryModeTransient, "1":
			return amqp.Transient, true
		case DeliveryModePersistent, "2":
			return amqp.Persistent, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// priorityFromHeader coerces a per-message "amqp091.priority" header
// override into an AMQP priority. Loop-back deliveries carry a typed uint8,
// but headers decoded from YAML/JSON route configs (or produced by another
// transport) arrive as int/int64/float64/string — the previous exact-uint8
// type assertion silently downgraded every such override to priority 0
// (`priority: 9` in YAML published at 0). Mirrors deliveryModeFromHeader's
// numeric coercion. Values outside 0-255 or non-integral floats are ignored
// so the broker default (0) applies rather than a wrong priority.
func priorityFromHeader(v any) (uint8, bool) {
	toPrio := func(n int64) (uint8, bool) {
		if n < 0 || n > 255 {
			return 0, false
		}
		return uint8(n), true
	}
	switch p := v.(type) {
	case uint8:
		return p, true
	case int:
		return toPrio(int64(p))
	case int8:
		return toPrio(int64(p))
	case int16:
		return toPrio(int64(p))
	case int32:
		return toPrio(int64(p))
	case int64:
		return toPrio(p)
	case uint16:
		return toPrio(int64(p))
	case uint32:
		return toPrio(int64(p))
	case uint64:
		if p > 255 {
			return 0, false
		}
		return toPrio(int64(p))
	case float64:
		if p != float64(int64(p)) {
			return 0, false
		}
		return toPrio(int64(p))
	case float32:
		return priorityFromHeader(float64(p))
	case string:
		s := strings.TrimSpace(p)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return toPrio(n)
	default:
		return 0, false
	}
}

// timestampFromHeader coerces a per-message "amqp091.timestamp" header
// override into an AMQP timestamp. Loop-back deliveries carry a typed
// time.Time, but route configs and other transports may supply a POSIX
// seconds integer or an RFC3339 string; the previous exact-time.Time
// assertion dropped those. Integers/floats are interpreted as seconds
// since the Unix epoch (the AMQP timestamp domain). Unparseable OR
// out-of-range values (seconds that would overflow int64 in time.Unix,
// which would otherwise yield a garbage pre-epoch timestamp) are ignored so
// no timestamp is published rather than a wrong one — parity with
// priorityFromHeader's range rejection.
func timestampFromHeader(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case int:
		return time.Unix(int64(t), 0).UTC(), true
	case int32:
		return time.Unix(int64(t), 0).UTC(), true
	case int64:
		return time.Unix(t, 0).UTC(), true
	case uint32:
		return time.Unix(int64(t), 0).UTC(), true
	case uint64:
		if t > uint64(math.MaxInt64) {
			return time.Time{}, false
		}
		return time.Unix(int64(t), 0).UTC(), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) ||
			t >= float64(math.MaxInt64) || t < float64(math.MinInt64) {
			return time.Time{}, false
		}
		return time.Unix(int64(t), 0).UTC(), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			return parsed.UTC(), true
		}
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}

// defaultDeliveryMode maps the sender's delivery_mode knob to the AMQP
// wire value applied when no per-message header override is present.
// Anything other than an explicit "transient" — including the empty
// string and invalid values that slipped past validation — resolves to
// persistent, because a publisher confirm for a transient message only
// means "in broker memory": the bridge would ack the source and the
// message would die with the destination broker.
func defaultDeliveryMode(cfg SenderConfig) uint8 {
	if cfg.DeliveryMode == DeliveryModeTransient {
		return amqp.Transient
	}
	return amqp.Persistent
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

	// Persistence: an explicit per-message header override (mapped by
	// headersToPublishing) wins; otherwise the sender's configured
	// delivery_mode default applies. Never publish with the unset zero
	// value — the broker treats 0 as transient.
	if pub.DeliveryMode == 0 {
		pub.DeliveryMode = defaultDeliveryMode(cfg)
	}

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
// the Sender. All SDK access — Confirm select-mode, deferred publisher
// confirmations, NotifyReturn, PublishWithDeferredConfirmWithContext — is
// encapsulated here so that the Sender stays SDK-free.
//
// Confirms are tracked via the SDK's DeferredConfirmation handles rather
// than a NotifyPublish listener channel. This is what makes pipelining
// safe: the SDK delivers confirmations to listener channels with a
// BLOCKING send from the connection's reader goroutine, so N in-flight
// publishes against a small listener buffer would stall the whole
// connection. Deferred confirmations are resolved map-side under the
// SDK's own lock and support any number of in-flight publishes.
//
// The wrapper deliberately preserves the original ordering guarantees
// of basic.return-before-basic.ack documented on checkReturn: the SDK
// dispatches basic.return to the (buffered) returns channel from the
// same serialized reader goroutine that later resolves the deferred
// confirmation, so by the time a confirm wait completes the matching
// return is already buffered.
//
// Concurrency: PublishConfirmed and PublishDeferred are NOT safe for
// concurrent use. The Sender serialises all publishing under its own
// mutex.
type senderChannel struct {
	ch        *amqpChannel
	returns   chan amqp.Return
	mandatory bool
}

// publisherChannel is the confirm-tracked publish surface the Sender drives on
// a cached AMQP channel. *senderChannel is the sole production implementation
// (all SDK access lives there). The interface is the seam that lets the
// publish-wedge timeout branch of Sender.Send — abandon the channel, release
// the mutex, hand the channel to the background reaper, return a transient
// error — be unit-tested with a blocking or failing publish and an observable
// Close, without a live broker. Only the operations the Sender actually calls
// are exposed; production wires it via openSenderChannel.
type publisherChannel interface {
	PublishConfirmed(ctx context.Context, exchange, routingKey string, mandatory bool, env *messaging.Envelope, cfg SenderConfig, clk clock.Clock) (publishResult, error)
	PublishDeferred(ctx context.Context, exchange, routingKey string, mandatory bool, env *messaging.Envelope, cfg SenderConfig, clk clock.Clock) (pendingPublish, error)
	IsClosed() bool
	Close() error
}

var _ publisherChannel = (*senderChannel)(nil)

// pendingPublish is the SDK-free handle for one in-flight pipelined publish
// awaiting its broker confirmation. *pendingConfirm is the sole production
// implementation; the interface is the seam that lets the batch confirm-drain
// logic (honour an already-settled confirm before an expired deadline) be
// unit-tested with a scriptable settled/wedged confirm, without a live broker.
type pendingPublish interface {
	DeliveryTag() uint64
	Settled() (done bool, err error)
	Wait(ctx context.Context) error
}

var _ pendingPublish = (*pendingConfirm)(nil)

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
		mandatory: mandatory,
	}
	if mandatory {
		sc.returns = ch.raw.NotifyReturn(make(chan amqp.Return, 1))
	}
	return sc, nil
}

// Close closes the underlying AMQP channel.
func (sc *senderChannel) Close() error { return sc.ch.Close() }

// IsClosed reports whether the underlying AMQP channel died out-of-band
// (asynchronous soft channel exception). The sender treats a cached
// channel that reports closed as stale and reopens on the next publish.
func (sc *senderChannel) IsClosed() bool { return sc.ch.IsClosed() }

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
// On any transport-level error (publish failure, ctx done, channel
// closed under the pending confirm) the channel is unusable and must be
// discarded by the caller (Sender.resetChannelLocked closes it).
func (sc *senderChannel) PublishConfirmed(
	ctx context.Context,
	exchange, routingKey string,
	mandatory bool,
	env *messaging.Envelope,
	cfg SenderConfig,
	clk clock.Clock,
) (publishResult, error) {
	pc, err := sc.PublishDeferred(ctx, exchange, routingKey, mandatory, env, cfg, clk)
	if err != nil {
		return publishResult{}, err
	}
	if err := pc.Wait(ctx); err != nil {
		return publishResult{}, err
	}
	res := publishResult{PublishOK: true, ConfirmedTag: pc.DeliveryTag()}
	if mandatory && sc.returns != nil {
		if rerr := checkReturn(sc.returns); rerr != nil {
			res.Returned = rerr
		}
	}
	return res, nil
}

// pendingConfirm is the SDK-free handle for one in-flight publish
// awaiting its broker confirmation. Obtained from PublishDeferred and
// settled with Wait; it lets the Sender pipeline a batch (publish N,
// then await N confirms) without observing SDK types.
type pendingConfirm struct {
	dc *amqp.DeferredConfirmation
	ch *amqpChannel
}

// DeliveryTag returns the broker delivery tag assigned to this publish.
func (p *pendingConfirm) DeliveryTag() uint64 { return p.dc.DeliveryTag }

// Settled reports, WITHOUT blocking, whether the broker has already settled
// this publish and, if so, its outcome: nil on a positive confirm,
// errPublishNacked on a broker nack, errConfirmChannelClosed when the channel
// died with the confirm outstanding. done=false means the confirm is still
// in flight. The pipelined batch loop uses it to honour an already-arrived
// confirm even after the batch deadline has expired, rather than losing a
// ready confirm to the ctx.Done() select race and misreporting a published
// message as transient (which would duplicate it on retry).
func (p *pendingConfirm) Settled() (done bool, err error) {
	select {
	case <-p.dc.Done():
	default:
		return false, nil
	}
	if p.dc.Acked() {
		return true, nil
	}
	if p.ch.raw.IsClosed() {
		return true, errConfirmChannelClosed
	}
	return true, errPublishNacked
}

// Wait blocks until the broker settles this publish or ctx expires.
// It returns nil on a positive confirm, errPublishNacked on a broker
// nack, errConfirmChannelClosed when the channel died with the confirm
// outstanding (the SDK nacks all pending confirms on channel close),
// and a wrapped ctx error on cancellation.
//
// Confirm-preferred: an already-settled confirm is honoured BEFORE ctx, so a
// confirm that arrived in the same instant the deadline fired is never lost to
// the SDK WaitContext's random select between ctx.Done() and the confirm
// channel — a lost race would misreport a delivered message as transient and
// duplicate it on retry.
func (p *pendingConfirm) Wait(ctx context.Context) error {
	if done, err := p.Settled(); done {
		return err
	}
	acked, err := p.dc.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for publish confirmation: %w", err)
	}
	if acked {
		return nil
	}
	if p.ch.raw.IsClosed() {
		return errConfirmChannelClosed
	}
	return errPublishNacked
}

// PublishDeferred publishes the envelope and returns a pendingConfirm
// handle without waiting for the broker confirmation. Callers pipeline
// throughput-critical batches by publishing every message first and
// awaiting the handles afterwards, collapsing N broker round-trips
// into one.
//
// On a publish error the channel is unusable and must be discarded by
// the caller (Sender.resetChannelLocked closes it).
//
// pendingPublish is an adapter-internal seam (category 5): returning the
// interface lets the pipelined batch confirm-drain be unit-tested with a
// scriptable settled/wedged confirm double, since a real *pendingConfirm wraps
// an SDK *amqp.DeferredConfirmation with unexported fields (not constructible
// in a hermetic test).
//
//nolint:ireturn // adapter-internal test seam (category 5); see comment above.
func (sc *senderChannel) PublishDeferred(
	ctx context.Context,
	exchange, routingKey string,
	mandatory bool,
	env *messaging.Envelope,
	cfg SenderConfig,
	clk clock.Clock,
) (pendingPublish, error) {
	pub := envelopeToPublishing(env, cfg, clk)

	// Defensive drain: under the deterministic ordering documented on
	// checkReturn every prior mandatory Send fully consumes its own
	// basic.return, so this loop should never find anything. If it
	// does, it indicates a code-path that bypassed the inspection
	// (e.g., a publish path with Mandatory toggled at runtime); drop
	// any such residue rather than mis-attribute it to the next Send.
	sc.drainReturns()

	dc, err := sc.ch.publishDeferredContext(ctx, exchange, routingKey, mandatory, pub)
	if err != nil {
		return nil, err
	}
	return &pendingConfirm{dc: dc, ch: sc.ch}, nil
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
