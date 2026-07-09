package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for AMQP 0-9-1.
// It publishes messages with publisher confirms enabled.
//
// All SDK access (channel open, NotifyPublish/NotifyReturn, publish,
// confirm/return inspection) is encapsulated in *senderChannel inside
// the ACL files; this struct only owns lifecycle state.
type Sender struct {
	cfg     SenderConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu sync.Mutex
	sc publisherChannel
	// scConn records the connection s.sc was opened on, so a channel
	// cached across a reconnect (stale connection) is detected and
	// replaced before the first post-reconnect publish.
	scConn amqpConnection

	// openChannel opens a fresh confirm-tracked publisher channel on conn.
	// It is a seam (defaults to openSenderChannel) so the publish-wedge
	// timeout branch can be unit-tested with a blocking/failing channel
	// without a live broker; production always wires openSenderChannel.
	openChannel func(conn amqpConnection, mandatory bool) (publisherChannel, error)
}

// NewSender creates a Sender bound to the given Session.
func NewSender(cfg SenderConfig) *Sender {
	cfg.applyDefaults()
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	clk := cfg.Clock
	if clk == nil && cfg.Session != nil {
		clk = cfg.Session.clk
	}
	if clk == nil {
		clk = clock.System
	}
	l := cfg.Logger
	if l == nil && cfg.Session != nil {
		l = cfg.Session.logger
	}
	return &Sender{
		cfg:     cfg,
		session: cfg.Session,
		logger:  l,
		metrics: m,
		clk:     clk,
		// Production channel-open. The seam default lives here so tests can
		// override it to inject a channel whose publish wedges; see
		// ensureChannelLocked.
		openChannel: func(conn amqpConnection, mandatory bool) (publisherChannel, error) {
			sc, err := openSenderChannel(conn, mandatory)
			if err != nil {
				return nil, err
			}
			return sc, nil
		},
	}
}

func (s *Sender) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Send publishes a single envelope to the AMQP broker with publisher
// confirms. The publish routing key is taken from msg.Address; when
// Address is empty, SenderConfig.RoutingKey is used as a fallback. The
// logical Envelope.Subject is propagated as the HeaderGobridgeSubject
// AMQP user header — it never selects the routing key. When neither
// msg.Address nor cfg.RoutingKey is set, Send returns
// shared.ErrInvalidTopic without contacting the broker.
//
// Precedence (Address first, then cfg default) mirrors the MQTT
// adapter, so a runtime-resolved per-dispatch destination always wins
// over the adapter's configured default.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("amqp091: nil envelope")
	}
	exchange := s.cfg.Exchange
	routingKey, err := resolveRoutingKey(s.cfg, msg)
	if err != nil {
		return err
	}

	// Fail fast when the broker has flow control engaged (connection.blocked
	// resource alarm). The SDK's PublishWithDeferredConfirmWithContext
	// discards ctx, so a publish issued while the broker is not reading the
	// socket blocks indefinitely — and it blocks while holding s.mu, wedging
	// every subsequent sender past its deadline. Refusing up front turns an
	// unbounded stall into a prompt, retryable ErrBrokerBusy the runtime can
	// act on. See Session.blockedState / watchBlocked.
	if s.session != nil {
		if blocked, reason := s.session.blockedState(); blocked {
			return shared.ErrBrokerBusy.WithMessage(
				"amqp091: broker flow control engaged, refusing publish: " + reason)
		}
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp091: publishing",
			"exchange", exchange,
			"routing_key", routingKey,
			"envelope_id", env.ID(),
			"payload_len", len(env.Payload()),
		)
	}

	sendCtx, cancel := s.applyTimeout(ctx)
	defer cancel()

	sessionTag := shared.Tag{Key: shared.TagKeyEntity, Value: s.cfg.Exchange}

	s.mu.Lock()
	sc, err := s.ensureChannelLocked()
	if err != nil {
		s.mu.Unlock()
		return MapError(err)
	}

	start := s.clock().Now()
	// Bound the publish by sendCtx even though the SDK's deferred-confirm
	// publish IGNORES ctx and blocks indefinitely while the broker holds
	// connection.blocked flow control (see Session.blockedState). awaitPublish
	// races the wedgeable publish against the deadline WITHOUT us holding s.mu
	// past it: on timeout we abandon the wedged channel — nil it so the next
	// publish opens a fresh one, and hand it to a background reaper that closes
	// it once the broker finally unblocks (Close serializes on the same SDK
	// send mutex the publish holds, so it too would wedge until then) — and
	// return a transient error so the runtime retries instead of the whole
	// sender (and shutdown/reconfig, which contends s.mu) stalling. The
	// pre-check above catches the steady-state blocked case; this handles the
	// race window before connection.blocked has propagated to blockedState.
	outcome, timedOut, done := awaitPublish(sendCtx, func() (publishResult, error) {
		return sc.PublishConfirmed(sendCtx, exchange, routingKey,
			s.cfg.Mandatory, env, s.cfg, s.clock())
	})
	if timedOut {
		s.sc = nil
		s.scConn = nil
		s.mu.Unlock()
		go reapWedgedChannel(done, sc)
		s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug,
				"amqp091: publish wedged past deadline; channel abandoned",
				"exchange", exchange, "routing_key", routingKey, "error", sendCtx.Err())
		}
		return mapPublishWedge(sendCtx)
	}
	res, perr := outcome.res, outcome.err
	if perr != nil {
		s.resetChannelLocked()
		s.mu.Unlock()
		s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp091: publish failed",
				"exchange", exchange, "routing_key", routingKey, "error", perr)
		}
		return mapPublishError(perr)
	}
	s.mu.Unlock()

	elapsed := s.clock().Since(start)
	s.metrics.Timer(MetricAMQP091PublishLatency, elapsed, sessionTag)

	if res.Returned != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug,
				"amqp091: mandatory message returned by broker",
				"reply_code", res.Returned.ReplyCode,
				"reply_text", res.Returned.ReplyText,
				"exchange", res.Returned.Exchange,
				"routing_key", res.Returned.RoutingKey,
			)
		}
		return shared.ErrNotFound.WithMessage(
			"amqp091: mandatory publish unroutable: " + res.Returned.ReplyText)
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp091: published",
			"exchange", exchange,
			"routing_key", routingKey,
			"envelope_id", env.ID(),
			"duration", elapsed,
		)
		s.logger.Log(ctx, logging.LevelTrace, "amqp091: publish confirmed",
			"delivery_tag", res.ConfirmedTag,
			"envelope_id", env.ID(),
		)
	}

	return nil
}

// mapPublishError converts the senderChannel sentinel errors and any
// transport-level error to a domain error. Sentinel sentinelError values
// from the ACL outbound code are translated to specific domain errors;
// everything else is run through MapError.
func mapPublishError(err error) error {
	if errors.Is(err, errConfirmChannelClosed) {
		return shared.ErrUnavailable.WithMessage("amqp091: confirm channel closed")
	}
	if errors.Is(err, errPublishNacked) {
		return shared.ErrUnavailable.WithMessage("amqp091: publish nacked by broker")
	}
	return MapError(err)
}

// SendBatch publishes every envelope and records the per-message
// outcome. The whole batch is dispatched — a failed publish does not
// abort the remaining messages. The returned slice is index-aligned
// with msgs and the error is always nil; see ports.BatchSender for the
// result contract.
//
// Non-mandatory batches are PIPELINED: all messages are published
// first, then the publisher confirms are awaited out-of-band, so the
// batch pays ~one broker round-trip instead of one per message (the
// difference between ~10 msg/s and wire-limited throughput on a 100ms
// WAN link). Mandatory batches fall back to sequential Send because
// unroutable-message attribution relies on the strict
// return-before-confirm ordering of one in-flight publish at a time
// (basic.return carries no delivery tag to match against).
func (s *Sender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) ([]ports.BatchResult, error) {
	// Fail fast under broker flow control (see Send): a blocked broker
	// wedges every publish under the sender mutex. Attribute ErrBrokerBusy
	// to each message so the caller retries the whole batch rather than
	// stalling past its deadline. Mirrors the per-message guard in Send.
	if s.session != nil {
		if blocked, reason := s.session.blockedState(); blocked {
			results := make([]ports.BatchResult, len(msgs))
			busy := shared.ErrBrokerBusy.WithMessage(
				"amqp091: broker flow control engaged, refusing publish: " + reason)
			for i := range msgs {
				results[i] = ports.BatchResult{Index: i, Err: busy}
			}
			return results, nil
		}
	}
	if s.cfg.Mandatory {
		results := make([]ports.BatchResult, len(msgs))
		for i, m := range msgs {
			results[i] = ports.BatchResult{Index: i, Err: s.Send(ctx, m)}
		}
		return results, nil
	}
	return s.sendBatchPipelined(ctx, msgs), nil
}

// sendBatchPipelined publishes all messages with deferred confirms and
// then awaits every confirmation. Per-message error attribution is
// preserved: a publish/validation failure is recorded at its own index
// and does not consume a confirmation; each confirm handle is awaited
// for exactly the message that produced it.
func (s *Sender) sendBatchPipelined(ctx context.Context, msgs []ports.OutboundMessage) []ports.BatchResult {
	results := make([]ports.BatchResult, len(msgs))
	if len(msgs) == 0 {
		return results
	}

	sendCtx, cancel := s.applyTimeout(ctx)
	defer cancel()

	sessionTag := shared.Tag{Key: shared.TagKeyEntity, Value: s.cfg.Exchange}

	type inflight struct {
		index int
		pc    *pendingConfirm
	}
	pending := make([]inflight, 0, len(msgs))

	s.mu.Lock()
	start := s.clock().Now()
	// blockedFrom marks the index at which broker flow control was detected
	// mid-batch; every message from there on is failed fast below.
	blockedFrom := -1
	blockedReason := ""
	i := 0
	for ; i < len(msgs); i++ {
		m := msgs[i]
		results[i] = ports.BatchResult{Index: i}
		if m.Envelope == nil {
			results[i].Err = shared.ErrInvalidPayload.WithMessage("amqp091: nil envelope")
			continue
		}
		// Re-check flow control per message. SendBatch only checks once at
		// entry, but a resource alarm can engage mid-batch; the next
		// PublishDeferred would then wedge under s.mu because the SDK's
		// deferred-publish path ignores ctx while the broker is not reading
		// the socket. Re-checking makes every not-yet-published message
		// fail fast with ErrBrokerBusy.
		//
		// Residual wedge bound: the connection.blocked signal is
		// asynchronous, so at most ONE publish — the one issued in the lag
		// window between flow control engaging and blockedState reflecting
		// it — can still wedge. Once blockedState is true this loop issues
		// no further publishes.
		if s.session != nil {
			if blocked, reason := s.session.blockedState(); blocked {
				blockedFrom = i
				blockedReason = reason
				break
			}
		}
		routingKey, err := resolveRoutingKey(s.cfg, m)
		if err != nil {
			results[i].Err = err
			continue
		}
		// (Re-)ensure the channel per message: a publish error discards
		// the channel, and the next message gets a fresh attempt instead
		// of inheriting a dead channel for the rest of the batch.
		sc, err := s.ensureChannelLocked()
		if err != nil {
			results[i].Err = MapError(err)
			continue
		}
		pc, perr := sc.PublishDeferred(sendCtx, s.cfg.Exchange, routingKey,
			false, m.Envelope, s.cfg, s.clock())
		if perr != nil {
			s.resetChannelLocked()
			results[i].Err = mapPublishError(perr)
			continue
		}
		pending = append(pending, inflight{index: i, pc: pc})
	}
	// Fail every message from the flow-control break point onward fast so
	// the caller retries them rather than stalling past its deadline.
	if blockedFrom >= 0 {
		busy := shared.ErrBrokerBusy.WithMessage(
			"amqp091: broker flow control engaged, refusing publish: " + blockedReason)
		for ; i < len(msgs); i++ {
			results[i] = ports.BatchResult{Index: i, Err: busy}
		}
	}

	// Await the confirms out-of-band, still under the sender mutex (the
	// channel and its confirmation bookkeeping are single-owner). On a
	// channel death the SDK nacks every outstanding confirm, so this
	// loop always terminates within the batch deadline.
	channelDead := false
	for _, p := range pending {
		if err := p.pc.Wait(sendCtx); err != nil {
			results[p.index].Err = mapPublishError(err)
			if errors.Is(err, errConfirmChannelClosed) ||
				errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				channelDead = true
			}
			continue
		}
		s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
	}
	if channelDead {
		s.resetChannelLocked()
	}
	s.mu.Unlock()

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp091: batch published",
			"exchange", s.cfg.Exchange,
			"batch_size", len(msgs),
			"pipelined", len(pending),
			"duration", s.clock().Since(start),
		)
	}
	return results
}

// ensureChannelLocked opens a channel if none is cached, and discards a
// channel that was cached on a now-stale connection. Caller must hold s.mu.
func (s *Sender) ensureChannelLocked() (publisherChannel, error) {
	if s.session == nil {
		return nil, shared.ErrUnavailable.WithMessage("amqp091: no session")
	}
	conn := s.session.connectionIfReady()
	if conn == nil {
		// nil covers both "never started" and the reconnect window
		// (connection installed but reconcile not yet complete). Both are
		// transient. The window this gate exists for is the MANDATORY
		// unroutable case: a mandatory publish to a not-yet-rebound exchange
		// comes back as a basic.return -> ErrNotFound (Permanent) and would
		// DLQ/drop a message that is fine to retry once reconcile rebinds.
		// (A missing-exchange 404 does NOT reach the publish path: the SDK
		// nacks the pending confirm via deferredConfirmations.Close(), so a
		// racing publish already sees a transient ErrUnavailable. A
		// permanently WRONG exchange name on a non-mandatory publish is
		// likewise classified transient by the SDK and loops until the
		// runtime replay cap — a pre-existing taxonomy fact tracked as an
		// ADR, not fixed here.)
		return nil, shared.ErrUnavailable.WithMessage("amqp091: session not connected")
	}
	// After a reconnect, the session exposes a fresh connection while s.sc
	// still wraps a channel on the old, closed one. Publishing on that
	// channel would fail spuriously (and only self-heal on the *next* send).
	// Detect the mismatch up front and reopen on the live connection so the
	// first post-reconnect publish succeeds. Also discard a channel that
	// died out-of-band on the SAME connection — a soft channel exception
	// (e.g. a 406/404 the SDK propagated asynchronously) closes only the
	// channel, leaving the connection live; without this the cached channel
	// would fail every subsequent publish until it happened to be reset.
	if s.sc != nil && (senderChannelStale(s.scConn, conn) || s.sc.IsClosed()) {
		s.resetChannelLocked()
	}
	if s.sc != nil {
		return s.sc, nil
	}

	sc, err := s.openChannel(conn, s.cfg.Mandatory)
	if err != nil {
		return nil, MapError(err)
	}
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelTrace,
			"amqp091: sender channel opened with confirms",
			"exchange", s.cfg.Exchange,
		)
	}
	s.sc = sc
	s.scConn = conn
	return sc, nil
}

// senderChannelStale reports whether a cached publisher channel opened on
// prevConn must be discarded because the session now exposes a different
// connection or the current connection has been closed. Pure (no SDK,
// no locks) so the staleness decision is unit-testable in isolation.
func senderChannelStale(prevConn, currentConn amqpConnection) bool {
	return prevConn != currentConn || currentConn == nil || currentConn.IsClosed()
}

// resetChannelLocked closes and clears the cached channel. Caller must hold s.mu.
func (s *Sender) resetChannelLocked() {
	if s.sc != nil {
		_ = s.sc.Close()
		s.sc = nil
	}
	s.scConn = nil
}

// Close closes the cached publisher channel. Safe to call multiple times.
func (s *Sender) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetChannelLocked()
	return nil
}

func (s *Sender) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, s.cfg.Timeout)
	}
	return ctx, func() {}
}

// resolveRoutingKey picks the AMQP 0-9-1 routing key for an outbound
// publish. Per-dispatch msg.Address wins over cfg.RoutingKey so that a
// runtime-resolved destination (e.g. a route-table lookup) overrides
// the adapter's configured default. When neither is set, the call is
// rejected with shared.ErrInvalidTopic; the logical Envelope.Subject
// is intentionally NOT consulted — Subject travels as a header, not as
// a transport address.
func resolveRoutingKey(cfg SenderConfig, msg ports.OutboundMessage) (string, error) {
	if msg.Address != "" {
		return msg.Address, nil
	}
	if cfg.RoutingKey != "" {
		return cfg.RoutingKey, nil
	}
	return "", shared.ErrInvalidTopic.WithMessage(
		"amqp091: no routing key specified and no default RoutingKey configured")
}

// publishOutcome carries the result of a single bounded publish attempt so
// awaitPublish can return it to the SDK-free Sender without leaking SDK types.
type publishOutcome struct {
	res publishResult
	err error
}

// awaitPublish runs publish — which may ignore ctx and block indefinitely, as
// the SDK's PublishWithDeferredConfirmWithContext does while the broker holds
// connection.blocked flow control — and races its completion against ctx. When
// the publish finishes first it returns the outcome with timedOut=false. When
// ctx fires first it returns timedOut=true and a done channel that closes once
// the wedged publish finally unblocks, so the caller can reap (force-close) the
// abandoned channel out-of-band WITHOUT holding the sender mutex meanwhile.
//
// Race-safety: the publish goroutine writes the outcome through o and then
// closes d; the non-timeout path reads *o only after receiving from d
// (close(d) establishes happens-before), and the timeout path never reads o.
func awaitPublish(
	ctx context.Context,
	publish func() (publishResult, error),
) (out publishOutcome, timedOut bool, done <-chan struct{}) {
	d := make(chan struct{})
	o := &publishOutcome{}
	go func() {
		o.res, o.err = publish()
		close(d)
	}()
	select {
	case <-d:
		return *o, false, d
	case <-ctx.Done():
		return publishOutcome{}, true, d
	}
}

// channelCloser is the minimal surface reapWedgedChannel needs; *senderChannel
// satisfies it. Keeping it an interface lets the reaper's ordering contract be
// unit-tested without a live broker channel.
type channelCloser interface{ Close() error }

// reapWedgedChannel force-closes a publisher channel abandoned by Send after a
// publish wedged past its deadline. It waits for the wedged publish goroutine
// to return (done closed) because the SDK's channel Close serializes on the
// same send mutex the publish holds — closing earlier would itself wedge. Run
// detached from the Send caller so a stuck broker never stalls the hot path,
// shutdown, or reconfig.
func reapWedgedChannel(done <-chan struct{}, sc channelCloser) {
	<-done
	_ = sc.Close()
}

// mapPublishWedge classifies a publish abandoned on deadline/cancel as a
// transient error so the runtime retries rather than DLQ-ing a message that
// merely hit broker flow control or an unresponsive connection.
func mapPublishWedge(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return shared.ErrUnavailable.WithMessage(
			"amqp091: publish canceled before broker confirmation; channel abandoned")
	}
	return shared.ErrTimeout.WithMessage(
		"amqp091: publish exceeded deadline before broker confirmation " +
			"(broker flow control or an unresponsive connection); channel abandoned to keep the sender responsive")
}
