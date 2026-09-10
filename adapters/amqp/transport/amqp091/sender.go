package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

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

	// abandoned counts publisher channels currently parked on background
	// reapers after a publish wedged past its deadline but whose wedged
	// publish has not yet returned (the reaper is still blocked on <-done).
	// It bounds the leak: on a broker with heartbeats the wedged
	// publishes unblock within the heartbeat read deadline and the reapers
	// decrement it, but with heartbeats disabled (or a black-holed network)
	// they could otherwise accumulate without bound. Once it reaches
	// cfg.MaxAbandonedPublishes, Send/SendBatch fast-fail transient instead of
	// stacking more wedged publishes. Atomic so the hot-path guard reads it
	// without taking s.mu.
	abandoned atomic.Int64

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

	// Back-pressure on the abandoned-publish budget: if too many
	// prior publishes wedged and their reapers have not drained yet, refuse
	// new publishes fast (transient) rather than stacking another wedged
	// channel on an unresponsive connection. The budget frees as the wedged
	// publishes unblock (broker recovery or reconnect).
	if s.abandonedBudgetExhausted() {
		return shared.ErrBrokerBusy.WithMessage(
			"amqp091: too many publishes wedged on an unresponsive connection, refusing until recovery")
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

	// Reserve one abandoned-publish slot BEFORE issuing the wedgeable publish
	// Reserving up-front — not charging after the
	// timeout unlocks — makes the cap a hard bound on already-admitted callers:
	// a caller that would push the reaper count past cfg.MaxAbandonedPublishes
	// is refused here rather than allowed to publish, wedge, and only then
	// discover the budget is blown. The reservation is released on any
	// non-wedged outcome (success, nack, channel-closed) and retained by the
	// reaper on a wedge/confirm-stall. Send holds s.mu across the whole publish
	// so healthy serialized publishes each take and release a single slot;
	// only genuinely parked reapers hold slots across callers.
	if !s.tryReserveAbandonedPublish() {
		s.mu.Unlock()
		return shared.ErrBrokerBusy.WithMessage(
			"amqp091: too many publishes wedged on an unresponsive connection, refusing until recovery")
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
		s.abandonReservedChannel(done, sc)
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
		// A ctx-derived publish/confirm error means the broker accepted the
		// publish but stalled before the publisher confirm. Closing
		// the channel synchronously (resetChannelLocked → sc.Close) would wait
		// for channel.close-ok on the SAME stalled broker and wedge s.mu,
		// blocking every future send, shutdown and reconfig. Route it through
		// the async abandon path exactly like the timed-out wedge: drop the
		// cached channel under lock, UNLOCK, then reap detached. done is closed
		// (the call returned), so the reaper closes immediately in the
		// background and only that goroutine — never s.mu — waits on the broker.
		if isPublishCtxError(perr) {
			s.sc = nil
			s.scConn = nil
			s.mu.Unlock()
			s.abandonReservedChannel(done, sc)
			s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug,
					"amqp091: publish confirm stalled; channel abandoned",
					"exchange", exchange, "routing_key", routingKey, "error", perr)
			}
			return mapPublishError(perr)
		}
		// Non-ctx error (nack, confirm channel closed, protocol error): the
		// broker responded, so Close returns promptly and is safe under s.mu.
		// Release the reservation and synchronously reset the dead channel.
		s.releaseAbandonedPublish()
		s.resetChannelLocked()
		s.mu.Unlock()
		s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp091: publish failed",
				"exchange", exchange, "routing_key", routingKey, "error", perr)
		}
		return mapPublishError(perr)
	}
	// Normal completion: the reservation taken up-front is no longer needed.
	s.releaseAbandonedPublish()
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
	// Abandoned-publish back-pressure: mirror Send's budget guard so
	// a batch cannot stack more wedged publishes once the reapers are saturated.
	if s.abandonedBudgetExhausted() {
		results := make([]ports.BatchResult, len(msgs))
		busy := shared.ErrBrokerBusy.WithMessage(
			"amqp091: too many publishes wedged on an unresponsive connection, refusing until recovery")
		for i := range msgs {
			results[i] = ports.BatchResult{Index: i, Err: busy}
		}
		return results, nil
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
		pc    pendingPublish
	}
	pending := make([]inflight, 0, len(msgs))

	s.mu.Lock()
	// Reserve one abandoned-publish slot for the whole batch BEFORE any
	// wedgeable publish, so a batch that would push the
	// reaper count past cfg.MaxAbandonedPublishes is refused up-front instead of
	// being allowed to publish, wedge, and only then blow the budget. A batch
	// abandons at most one channel (it stops at the first publish wedge, and a
	// confirm-stall abandons the single live channel), so one reservation
	// suffices. handedOff records whether that slot was handed to a reaper; if
	// not, it is released before unlock.
	if !s.tryReserveAbandonedPublish() {
		s.mu.Unlock()
		busy := shared.ErrBrokerBusy.WithMessage(
			"amqp091: too many publishes wedged on an unresponsive connection, refusing until recovery")
		for i := range msgs {
			results[i] = ports.BatchResult{Index: i, Err: busy}
		}
		return results
	}
	handedOff := false
	start := s.clock().Now()
	// tailErr, when set, fails message i and every message after it fast and
	// stops the publish loop: either broker flow control engaged mid-batch
	// (ErrBrokerBusy) or a publish wedged past the deadline (mapPublishWedge).
	// Both mean we must issue no further publishes on this batch and let the
	// caller retry the unsent tail.
	var tailErr error
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
		// it — can still wedge. The awaitCall bound below catches THAT one so
		// it cannot stall the batch (and s.mu) past the deadline.
		if s.session != nil {
			if blocked, reason := s.session.blockedState(); blocked {
				tailErr = shared.ErrBrokerBusy.WithMessage(
					"amqp091: broker flow control engaged, refusing publish: " + reason)
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
		// Bound the deferred publish by sendCtx, exactly like Send: the SDK's
		// PublishWithDeferredConfirmWithContext IGNORES ctx and blocks while
		// the broker holds connection.blocked flow control. Issuing it raw
		// under s.mu (as before) let a single mid-batch resource alarm wedge
		// the batch AND every subsequent send, shutdown, and reconfiguration
		// on the sender mutex. awaitCall runs it on a goroutine raced against
		// sendCtx; on the deadline we abandon the channel to a reaper and stop.
		pubOut, timedOut, done := awaitCall(sendCtx, func() (pendingPublish, error) {
			return sc.PublishDeferred(sendCtx, s.cfg.Exchange, routingKey,
				false, m.Envelope, s.cfg, s.clock())
		})
		if timedOut {
			// Drop the wedged channel so a later send reopens a fresh one and
			// hand it to the background reaper against the reserved budget slot.
			// Fail this message and the unsent tail transient. The already-
			// pending confirms are drained below (Settled-first) so any the
			// broker already confirmed keep their real outcome; genuinely
			// unsettled ones under the now-expired sendCtx fail transient.
			s.sc = nil
			s.scConn = nil
			s.abandonReservedChannel(done, sc)
			handedOff = true
			tailErr = mapPublishWedge(sendCtx)
			break
		}
		pc, perr := pubOut.val, pubOut.err
		if perr != nil {
			// The SDK's deferred-publish IGNORES ctx, so this error is never a
			// ctx timeout — it is a real publish failure (dead channel, protocol
			// error). The broker responded, so Close returns promptly and
			// resetChannelLocked is safe under s.mu (unlike the confirm-stall
			// path below, which must abandon asynchronously).
			s.resetChannelLocked()
			results[i].Err = mapPublishError(perr)
			continue
		}
		pending = append(pending, inflight{index: i, pc: pc})
	}
	// Fail every message from the break point onward fast so the caller
	// retries them rather than stalling past its deadline.
	if tailErr != nil {
		for ; i < len(msgs); i++ {
			results[i] = ports.BatchResult{Index: i, Err: tailErr}
		}
	}

	// Await the confirms out-of-band, still under the sender mutex (the
	// channel and its confirmation bookkeeping are single-owner). On a
	// channel death the SDK nacks every outstanding confirm, so this
	// loop always terminates within the batch deadline.
	//
	// ponytail: confirm-drain is confirm-preferred, bounded at-least-once.
	// A message whose confirm already arrived is recorded with its real
	// outcome via Settled() BEFORE the (possibly expired) sendCtx is honoured
	// otherwise a delivered message loses the ctx.Done() select
	// race and is misreported transient, duplicating on retry. The residual
	// ceiling: publishes whose confirms are GENUINELY still in flight when the
	// deadline fires are ambiguous (broker may or may not have persisted them)
	// and are reported transient, so a retry may duplicate that unconfirmed
	// prefix. This is inherent to at-least-once publishing under a hard
	// deadline; the bound is the number of unsettled in-flight confirms.
	channelDead := false
	confirmWedged := false
	var wedgedSC channelCloser
	for _, p := range pending {
		if settled, serr := p.pc.Settled(); settled {
			if serr != nil {
				results[p.index].Err = mapPublishError(serr)
				if errors.Is(serr, errConfirmChannelClosed) {
					channelDead = true
				}
				continue
			}
			s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
			continue
		}
		if err := p.pc.Wait(sendCtx); err != nil {
			results[p.index].Err = mapPublishError(err)
			switch {
			case isPublishCtxError(err):
				// The broker accepted the publish but stalled before the
				// confirm. Closing the channel synchronously would
				// wait for channel.close-ok on the SAME stalled broker under
				// s.mu, wedging every future send/shutdown/reconfig. Abandon it
				// asynchronously after the loop instead. The stalled channel is
				// the live cached one (a confirm on an already-reset channel
				// would have been nacked, not ctx-wedged).
				confirmWedged = true
				if wedgedSC == nil {
					wedgedSC = s.sc
				}
			case errors.Is(err, errConfirmChannelClosed):
				channelDead = true
			}
			continue
		}
		s.metrics.Timer(MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
	}
	switch {
	case confirmWedged && !handedOff:
		// Drop the stalled channel under lock, then reap it detached so the
		// broker's channel.close-ok wait never blocks s.mu. done is nil: the
		// publish already returned, only the confirm stalled.
		s.sc = nil
		s.scConn = nil
		s.abandonReservedChannel(nil, wedgedSC)
		handedOff = true
	case channelDead:
		// Broker closed the channel (all pending confirms nacked): Close is
		// prompt, so reset synchronously.
		s.resetChannelLocked()
	}
	if !handedOff {
		s.releaseAbandonedPublish()
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

// awaitResult carries the outcome of a bounded call raced against ctx.
type awaitResult[T any] struct {
	val T
	err error
}

// awaitCall runs call — which may ignore ctx and block indefinitely, as the
// SDK's PublishWithDeferredConfirmWithContext does while the broker holds
// connection.blocked flow control — on a goroutine and races its completion
// against ctx. When the call finishes first it returns the result with
// timedOut=false. When ctx fires first it returns timedOut=true and a done
// channel that closes once the wedged call finally unblocks, so the caller can
// reap (force-close) the abandoned channel out-of-band WITHOUT holding the
// sender mutex meanwhile.
//
// Race-safety: the goroutine writes *o and then closes d; the non-timeout path
// reads *o only after receiving from d (close(d) establishes happens-before),
// and the timeout path never reads o.
func awaitCall[T any](
	ctx context.Context,
	call func() (T, error),
) (out awaitResult[T], timedOut bool, done <-chan struct{}) {
	d := make(chan struct{})
	o := &awaitResult[T]{}
	go func() {
		o.val, o.err = call()
		close(d)
	}()
	select {
	case <-d:
		return *o, false, d
	case <-ctx.Done():
		return awaitResult[T]{}, true, d
	}
}

// awaitPublish is the confirmed-publish (Send) specialisation of awaitCall.
// See awaitCall for the timeout/abandon/reap contract and race-safety.
func awaitPublish(
	ctx context.Context,
	publish func() (publishResult, error),
) (out publishOutcome, timedOut bool, done <-chan struct{}) {
	res, timedOut, d := awaitCall(ctx, publish)
	return publishOutcome{res: res.val, err: res.err}, timedOut, d
}

// channelCloser is the minimal surface reapWedgedChannel needs; *senderChannel
// satisfies it. Keeping it an interface lets the reaper's ordering contract be
// unit-tested without a live broker channel.
type channelCloser interface{ Close() error }

// reapWedgedChannel force-closes a publisher channel abandoned after a publish
// wedged past its deadline (or after a publisher confirm stalled on a half-dead
// broker). When done is non-nil it first waits for the wedged publish goroutine
// to return (done closed) because the SDK's channel Close serializes on the
// same send mutex the publish holds — closing earlier would itself wedge. When
// done is nil the publish already returned (only the confirm was outstanding),
// so it closes immediately. Run detached from the caller so a stuck broker
// never stalls the hot path, shutdown, or reconfig.
func reapWedgedChannel(done <-chan struct{}, sc channelCloser) {
	if done != nil {
		<-done
	}
	if sc != nil {
		_ = sc.Close()
	}
}

// tryReserveAbandonedPublish atomically reserves one slot of the per-sender
// abandoned-publish budget BEFORE a wedgeable publish is issued, so
// the cap bounds already-admitted concurrent callers rather than only callers
// that arrive after exhaustion. It returns false when the budget is already at
// cfg.MaxAbandonedPublishes (a non-positive cap disables the guard). The CAS
// loop makes check-and-charge a single atomic step: a normal publish releases
// the reservation on completion (releaseAbandonedPublish); a wedged one keeps
// it charged until its reaper drains (abandonReservedChannel).
func (s *Sender) tryReserveAbandonedPublish() bool {
	limit := int64(s.cfg.MaxAbandonedPublishes)
	for {
		cur := s.abandoned.Load()
		if limit > 0 && cur >= limit {
			return false
		}
		if s.abandoned.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// releaseAbandonedPublish returns a reservation taken by
// tryReserveAbandonedPublish when the publish completed normally (no wedge).
func (s *Sender) releaseAbandonedPublish() {
	s.abandoned.Add(-1)
}

// abandonReservedChannel hands a channel whose publish wedged, or whose
// publisher confirm stalled on a half-dead broker, to a background reaper. The
// reservation was already taken (tryReserveAbandonedPublish) so it does NOT
// re-charge; the reaper closes the channel once the wedged publish returns
// (done non-nil) or immediately (done nil, confirm-only stall) and then
// RELEASES the reservation. Until then the live count bounds how many such
// leaks accumulate: Send/SendBatch refuse new publishes once the budget is
// exhausted, so a black-holed connection cannot stack reapers without bound.
func (s *Sender) abandonReservedChannel(done <-chan struct{}, sc channelCloser) {
	go func() {
		reapWedgedChannel(done, sc)
		s.releaseAbandonedPublish()
	}()
}

// abandonedBudgetExhausted reports whether the number of reserved abandoned-
// publish slots has reached cfg.MaxAbandonedPublishes. A non-positive cap
// disables the guard (unlimited). This is a cheap pre-lock fast-fail; the hard
// cap is enforced under s.mu by tryReserveAbandonedPublish.
func (s *Sender) abandonedBudgetExhausted() bool {
	limit := s.cfg.MaxAbandonedPublishes
	return limit > 0 && s.abandoned.Load() >= int64(limit)
}

// isPublishCtxError reports whether a publish-or-confirm error is derived from
// ctx cancellation/deadline (directly or wrapped, e.g. through pendingConfirm.
// Wait's "%w" wrap). Such an error means the broker accepted the publish but
// stalled before the publisher confirm: closing the channel synchronously would
// wait for channel.close-ok on the same stalled broker and wedge s.mu, so these
// are routed through the async abandon path instead of resetChannelLocked.
func isPublishCtxError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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
