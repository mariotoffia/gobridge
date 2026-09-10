package amqp10

import (
	"context"
	"errors"
	"log/slog"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for AMQP 1.0 links.
type Receiver struct {
	cfg     ReceiverConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	started     chan struct{}
	startedOnce sync.Once

	mu       sync.Mutex
	link     linkReceiver
	linkConn amqpConn

	// In-flight settlement tracking.
	// inflightCount counts deliveries emitted to the pipeline whose
	// settlement has not yet completed; inflightIdle is closed on every
	// transition to zero so Close can wait event-driven (bounded by its
	// ctx) for outstanding Acks/Retries before detaching the link.
	inflightMu    sync.Mutex
	inflightCount int
	inflightIdle  chan struct{}

	// Failed-settlement tracking. Each failed settlement
	// permanently consumes one link-credit slot (go-amqp replenishes
	// credit only on a completed disposition), so once LinkCredit slots
	// are gone the broker stops delivering and the receive loop blocks
	// forever with Health still Full. settleFailures counts failures on
	// the CURRENT link; at settleFailureThreshold the receiver forces a
	// link rebuild — the broker redelivers the still-unsettled messages,
	// reissuing their credit — and the count resets.
	settleFailMu   sync.Mutex
	settleFailures int
	// linkGeneration stamps each successful createLink so a stale
	// in-flight delivery from a PRIOR link/connection (which can fire
	// onSettleFailed AFTER createLink reset settleFailures for the new
	// link) is not miscounted against the current link and cannot trip a
	// spurious extra rebuild. Guarded by settleFailMu.
	linkGeneration int
}

// NewReceiver creates an AMQP 1.0 Receiver.
//
// LOW-LEVEL constructor. It builds and validates a Receiver but does NOT
// enforce the durable-subscription safety gates — the explicit-container_id
// requirement and the dedicated-session contract are
// enforced by Factory.NewReceiver, which is the ONLY path production uses
// (bridge/runtime build every link through the ports.TransportFactory
// interface; see Factory.NewReceiver). This constructor stays permissive so
// tests can build durable links directly on shared sessions to exercise
// teardown behaviour. Do NOT call it directly for a production durable
// receiver: use Factory.NewReceiver so the gates apply.
func NewReceiver(cfg ReceiverConfig, session *Session) (*Receiver, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l := cfg.Logger
	if l == nil && session != nil {
		l = session.logger
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	clk := cfg.Clock
	if clk == nil && session != nil {
		clk = session.clock()
	}
	if clk == nil {
		clk = clock.System
	}
	return &Receiver{
		cfg:     cfg,
		session: session,
		logger:  l,
		metrics: m,
		clk:     clk,
		started: make(chan struct{}),
	}, nil
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the receiver's link
// has been created and the receive loop is live.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// Run creates a receiver link and enters the receive loop.
//
// Cold-start resilience: a RECOVERABLE initial link failure (broker
// briefly unreachable while the session manager is still dialing) does
// NOT fail Run — by the ports.Receiver contract a non-ctx error from
// Run is terminal for the whole runtime. Instead the receive loop's
// waitAndReconnect path (the same one used for mid-run link loss)
// establishes the link once the session connects. Only permanent
// (misconfiguration-class) errors surface immediately.
//
// The receiver link is deliberately left OPEN when Run returns: the
// route runner settles in-flight deliveries AFTER Run exits and then
// calls Close, which waits (bounded) for those settlements before
// detaching. Closing the link here would fail every in-flight Ack on
// graceful shutdown and cause duplicates after restart.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "amqp10: receiver starting",
			"address", redactURL(r.cfg.Address),
			"link_credit", r.cfg.LinkCredit,
		)
	}

	if r.session != nil {
		// Register for health tracking so Session.Health can
		// report Degraded if this receiver's link detaches while the
		// session connection itself is still alive.
		r.session.registerReceiver(r)
		defer r.session.unregisterReceiver(r)
	}

	if err := r.ensureLink(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !shared.IsRecoverableError(err) {
			return err
		}
		if r.logger != nil {
			r.logger.Warn("amqp10: initial link creation failed; waiting for session",
				"address", redactURL(r.cfg.Address),
				"error", err,
			)
		}
	}

	return r.receiveLoop(ctx, emit)
}

// Close waits — bounded by ctx — for in-flight delivery settlements to
// complete, then detaches the receiver link. The route runner calls it
// after Run has returned and its processing goroutines have drained, so
// graceful shutdown never strands an Ack on a closed link.
func (r *Receiver) Close(ctx context.Context) error {
	r.waitInflight(ctx)
	r.closeLink()
	return nil
}

// trackDelivery registers del in the in-flight settlement count and
// arms its onSettled hook to decrement on completion. It also arms the
// onSettleFailed hook so a settlement failure feeds the leaked-credit
// watchdog.
func (r *Receiver) trackDelivery(del *Delivery) {
	r.inflightMu.Lock()
	if r.inflightCount == 0 {
		r.inflightIdle = make(chan struct{})
	}
	r.inflightCount++
	r.inflightMu.Unlock()
	del.onSettled = r.settlementDone
	// bind the failure hook to the link generation live at track
	// time so a settlement completing after a later rebuild is recognised
	// as stale and not counted against the new link.
	r.settleFailMu.Lock()
	gen := r.linkGeneration
	r.settleFailMu.Unlock()
	del.onSettleFailed = func(err error) { r.settlementFailed(gen, err) }
}

// settlementFailed is the Delivery.onSettleFailed hook. It records the
// failure metric and, once accumulated failures on the current link
// reach the credit-safety threshold, forces a link rebuild so the leaked
// link credit is reclaimed before the receiver stalls. gen
// is the link generation captured when the delivery was tracked.
func (r *Receiver) settlementFailed(gen int, cause error) {
	// a context.Canceled cause is a deliberate route teardown /
	// reconfig, not a broker-health signal. Counting it could trip a
	// spurious rebuild (the durable branch drops the WHOLE connection), so
	// return before touching the counter or emitting the failure metric.
	// context.DeadlineExceeded is deliberately NOT exempted — it can be a
	// genuinely stuck broker that the watchdog must still react to.
	if errors.Is(cause, context.Canceled) {
		return
	}

	r.metrics.Counter(MetricAMQP10SettleFailed, 1,
		shared.Tag{Key: shared.TagKeyEntity, Value: r.cfg.Address})

	r.settleFailMu.Lock()
	// ignore failures from a superseded link generation. A stale
	// in-flight delivery from the previous link/connection must not be
	// counted against the freshly rebuilt link (which would trip an
	// immediate second rebuild). The metric above still fires so the stale
	// settle failure remains observable.
	if gen != r.linkGeneration {
		r.settleFailMu.Unlock()
		return
	}
	r.settleFailures++
	trip := r.settleFailures >= r.settleFailureThreshold()
	if trip {
		r.settleFailures = 0
	}
	r.settleFailMu.Unlock()

	if trip {
		r.forceSettleRebuild(cause)
	}
}

// settleFailureThreshold is the number of failed settlements on one link
// that forces a rebuild. It is half the configured link credit (min 1)
// so the rebuild happens BEFORE every credit slot is leaked and the
// receiver blocks. LinkCredit defaults to 10 → threshold 5.
func (r *Receiver) settleFailureThreshold() int {
	t := int(r.cfg.LinkCredit) / 2
	if t < 1 {
		t = 1
	}
	return t
}

// forceSettleRebuild tears the current receiver link down so the blocked
// receive loop wakes, re-attaches, and the broker redelivers the
// unsettled messages (reissuing their leaked credit). It runs from a
// settlement goroutine via settlementFailed, so it must be safe against a
// concurrent receive loop. A non-durable link is closed directly (which
// unblocks link.Receive with a transient link error → the loop rebuilds
// on the live session). A durable subscription link must NOT be closed —
// go-amqp can only full-close it, which brokers read as UNSUBSCRIBE — so
// the connection is dropped instead (the link dies with it and the
// monitor reconnects with the subscription intact). Finding.
func (r *Receiver) forceSettleRebuild(cause error) {
	r.mu.Lock()
	link := r.link
	r.link = nil
	failedConn := r.linkConn
	r.linkConn = nil
	r.mu.Unlock()

	if link == nil {
		return // a rebuild is already underway
	}

	if r.logger != nil {
		r.logger.Warn("amqp10: forcing receiver link rebuild after repeated settlement failures (leaked link credit)",
			"address", redactURL(r.cfg.Address),
			"error", cause,
		)
	}
	if r.session != nil {
		r.session.noteLinkError(cause)       // Surface cause in Health
		r.session.markReceiverLink(r, false) // Link is down
	}

	if r.cfg.DurabilityMode == 0 {
		closeCtx, cancel := context.WithTimeout(context.Background(), r.linkCloseTimeout())
		_ = link.Close(closeCtx)
		cancel()
		return
	}
	if r.session != nil && failedConn != nil {
		r.session.notifyDisconnect(failedConn, cause)
	}
}

// settlementDone is the Delivery.onSettled hook: it decrements the
// in-flight count and signals idle on the transition to zero.
func (r *Receiver) settlementDone() {
	r.inflightMu.Lock()
	r.inflightCount--
	if r.inflightCount == 0 && r.inflightIdle != nil {
		close(r.inflightIdle)
		r.inflightIdle = nil
	}
	r.inflightMu.Unlock()
}

// waitInflight blocks until every tracked delivery has settled or ctx
// expires (the bound that keeps Close from hanging on a lost settler).
func (r *Receiver) waitInflight(ctx context.Context) {
	for {
		r.inflightMu.Lock()
		if r.inflightCount == 0 {
			r.inflightMu.Unlock()
			return
		}
		idle := r.inflightIdle
		pending := r.inflightCount
		r.inflightMu.Unlock()

		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug,
				"amqp10: waiting for in-flight settlements before link close",
				"address", redactURL(r.cfg.Address),
				"in_flight", pending,
			)
		}

		select {
		case <-idle:
		case <-ctx.Done():
			if r.logger != nil {
				r.logger.Warn("amqp10: closing link with unsettled in-flight deliveries",
					"address", redactURL(r.cfg.Address),
					"in_flight", pending,
				)
			}
			return
		}
	}
}

func (r *Receiver) closeLink() {
	r.mu.Lock()
	link := r.link
	r.link = nil
	failedConn := r.linkConn
	r.linkConn = nil
	r.mu.Unlock()

	if link == nil {
		return
	}

	// Durable subscriptions (DurabilityMode > 0) must NOT be full-closed
	// at the link level: go-amqp can only send a CLOSING detach
	// (Detach{Closed:true}), which brokers interpret as UNSUBSCRIBE —
	// deleting the subscription and every message retained for it
	// (verified against Artemis: a closing detach deletes, a connection
	// drop — a NON-closing detach — preserves the durable terminus).
	//
	// Merely nil-ing r.link (the previous behaviour) is
	// NOT a teardown — the link stays ATTACHED on the broker, which keeps
	// delivering up to link credit into an abandoned link whose messages
	// then sit UNSETTLED until the connection eventually drops (possibly
	// never, while the session serves other links). Force a REAL
	// connection teardown instead: dropping the connection detaches the
	// live link (broker stops delivering) via a non-closing detach that
	// PRESERVES the durable subscription. This mirrors the durable branch
	// of forceSettleRebuild — the same seam used to reclaim leaked credit.
	if r.cfg.DurabilityMode > 0 {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(context.Background(), logging.LevelDebug,
				"amqp10: tearing down connection to detach durable subscription link on close (link close would unsubscribe)",
				"address", redactURL(r.cfg.Address),
				"subscription", r.linkName(),
			)
		}
		if r.session != nil {
			r.session.markReceiverLink(r, false) // Link is down
			if failedConn != nil {
				// notifyDisconnect closes failedConn, clears session
				// connection state and wakes the monitor; it no-ops when
				// the session is already closed or has moved on to a newer
				// connection, so a concurrent Session.Close stays safe.
				r.session.notifyDisconnect(failedConn, nil)
			}
		}
		return
	}
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace, "amqp10: closing receiver link",
			"address", redactURL(r.cfg.Address))
	}
	// Bound the detach. closeLink runs from Close on
	// shutdown; an unbounded context.Background() could hang the
	// caller forever on an unresponsive broker. We derive from
	// Background (not a — by now cancelled — Run ctx) on purpose so
	// the detach still gets its full LinkCloseTimeout window to flush
	// in-flight dispositions during a graceful stop, mirroring
	// handleLinkError.
	closeCtx, cancel := context.WithTimeout(context.Background(), r.linkCloseTimeout())
	_ = link.Close(closeCtx)
	cancel()
}

// defaultLinkCloseTimeout bounds a link detach when SessionOptions does
// not supply one. NewSession always calls applyDefaults (which sets 5s),
// so this only guards directly-constructed receivers in tests.
const defaultLinkCloseTimeout = 5 * time.Second

// linkCloseTimeout returns the bounded deadline used to detach a receiver
// link. A non-positive or unset SessionOptions.LinkCloseTimeout falls
// back to defaultLinkCloseTimeout so link.Close can never block the Run
// goroutine indefinitely on an unresponsive broker.
func (r *Receiver) linkCloseTimeout() time.Duration {
	if r.session != nil && r.session.opts.LinkCloseTimeout > 0 {
		return r.session.opts.LinkCloseTimeout
	}
	return defaultLinkCloseTimeout
}

func (r *Receiver) ensureLink(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.link != nil {
		return nil
	}

	return r.createLink(ctx)
}

func (r *Receiver) createLink(ctx context.Context) error {
	// Capture the session link and its owning connection under
	// ONE session lock so the (link, conn) pair can never be mismatched
	// by a concurrent reconnect between two separate getter calls. A
	// stale pairing would make notifyDisconnect drop a later legitimate
	// disconnect notification for this link's real connection.
	sess, conn := r.session.sessionAndConn()
	if sess == nil {
		return shared.ErrUnavailable.WithMessage("amqp10: session not connected")
	}

	recv, err := sess.NewReceiverLink(
		ctx,
		r.cfg.Address,
		int32(r.cfg.LinkCredit),
		r.cfg.DurabilityMode,
		r.cfg.Routing.capability(),
		r.linkName(),
	)
	if err != nil {
		return MapError(err)
	}

	r.link = recv
	r.linkConn = conn
	// A fresh link starts with full credit; clear any settlement-failure
	// count carried from the previous link so the watchdog counts only
	// failures on THIS link, and bump the link generation so stale
	// in-flight settlements from the previous link are ignored.
	r.settleFailMu.Lock()
	r.settleFailures = 0
	r.linkGeneration++
	r.settleFailMu.Unlock()
	if r.session != nil {
		r.session.markReceiverLink(r, true) // Link is up
	}
	r.startedOnce.Do(func() { close(r.started) })
	return nil
}

// linkName returns the AMQP link name for this receiver. An explicit
// SubscriptionName always wins. Durable subscriptions (DurabilityMode >
// 0) REQUIRE a stable name — brokers identify the subscription by
// container-id + link name, so the SDK's random default would orphan
// the subscription on every reconnect and silently drop everything
// published while detached — hence a deterministic name is derived from
// the session container id and the link address. Non-durable links keep
// the SDK's random name (empty return).
func (r *Receiver) linkName() string {
	if r.cfg.SubscriptionName != "" {
		return r.cfg.SubscriptionName
	}
	if r.cfg.DurabilityMode == 0 {
		return ""
	}
	cid := ""
	if r.session != nil {
		cid = r.session.opts.ContainerID
	}
	if cid == "" {
		// Only reachable for directly-constructed sessions (tests):
		// SessionOptions.applyDefaults generates a per-instance
		// container-id when none is configured, so sessions built via
		// NewSession always have one. Production durable receivers built
		// through the factory always carry an EXPLICIT container_id —
		// Factory.NewReceiver rejects durability_mode > 0 on a session with
		// a generated container-id — so this generated-identity
		// fallback never anchors a real durable subscription.
		return "gobridge:" + r.cfg.Address
	}
	return cid + ":" + r.cfg.Address
}

func (r *Receiver) receiveLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	receiveBackoff := newBackoff()
	// linkBackoff throttles link RE-CREATION attempts. Without it, a
	// connected-but-attach-refusing broker (e.g. resource-limit-exceeded)
	// turns the waitAndReconnect path into a tight unthrottled attach
	// storm: Health reports Connected, so waitAndReconnect returns
	// immediately and the loop re-attaches as fast as the broker can
	// refuse.
	linkBackoff := newBackoff()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		r.mu.Lock()
		link := r.link
		r.mu.Unlock()

		if link == nil {
			if err := r.waitAndReconnect(ctx); err != nil {
				return err
			}
			r.mu.Lock()
			created := r.link != nil
			r.mu.Unlock()
			if created {
				linkBackoff.reset()
				continue
			}
			delay := linkBackoff.next()
			if r.logger != nil {
				r.logger.Warn("amqp10: link re-creation failed, backing off",
					"address", redactURL(r.cfg.Address),
					"retry_after", delay,
				)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.clock().After(delay):
			}
			continue
		}

		del, err := link.Receive(ctx, r.logger, r.metrics, r.clock())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if errors.Is(err, errIngressRejected) {
				// A malformed message was rejected at the broker inside
				// Receive. The link is healthy and the message settled —
				// count it and keep receiving; one poison message must
				// never exit the loop (a non-transient return would take
				// the whole bridge down).
				r.metrics.Counter(MetricAMQP10IngressRejected, 1,
					shared.Tag{Key: shared.TagKeyEntity, Value: r.cfg.Address})
				if r.logger != nil {
					r.logger.Warn("amqp10: rejected malformed inbound message",
						"address", redactURL(r.cfg.Address),
						"error", err,
					)
				}
				continue
			}

			bridgeErr := MapError(err)
			if bridgeErr != nil && bridgeErr.Class != shared.ErrorTransient {
				r.handleLinkError(err)
				return bridgeErr
			}

			delay := receiveBackoff.next()
			if r.logger != nil {
				r.logger.Warn("amqp10: receive failed, retrying",
					"address", redactURL(r.cfg.Address),
					"error", err,
					"retry_after", delay,
				)
			}

			r.handleLinkError(err)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.clock().After(delay):
			}
			continue
		}

		receiveBackoff.reset()

		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "amqp10: received message",
				"address", redactURL(r.cfg.Address),
				"message_id", del.Envelope().ID(),
				"body_len", len(del.Envelope().Payload()),
			)
		}

		r.trackDelivery(del)
		if err := emit(ctx, del); err != nil {
			// The pipeline did not take ownership: release the in-flight
			// slot so Close does not wait on a settlement nobody will
			// perform. The delivery itself stays settleable (ownership
			// contract: settlement remains with whoever holds it).
			del.fireOnSettled()
			return err
		}
	}
}

// handleLinkError detaches the receiver's link after a receive-loop
// failure. A NON-durable link is closed and rebuilt in isolation; only a
// connection-scoped error escalates to a session teardown. A DURABLE link
// is a special case: it can never be individually re-attached on a live
// connection, so ANY error forces a full connection teardown (see the
// durable branch below).
func (r *Receiver) handleLinkError(err error) {
	r.mu.Lock()
	link := r.link
	r.link = nil
	failedConn := r.linkConn
	r.linkConn = nil
	r.mu.Unlock()

	if r.session != nil {
		r.session.noteLinkError(err)         // Surface cause in Health
		r.session.markReceiverLink(r, false) // Link is down
	}

	if r.cfg.DurabilityMode > 0 {
		// A durable subscription link can NEVER be individually re-attached
		// on a live connection: go-amqp can only emit a CLOSING detach
		// (Detach{Closed:true}) = broker UNSUBSCRIBE, which destroys the
		// terminus and every retained message. So the failed durable link
		// is abandoned locally but STAYS attached on the wire. Re-creating
		// it on the same connection would collide with the still-attached
		// link, and the credit the broker holds for any unsettled delivery
		// on it could NEVER be reissued — e.g. a malformed message whose
		// reject settlement FAILED, which MapError classifies as a
		// transient ErrTimeout, NOT ConnectionLost/Unavailable, so the
		// generic escalation below would leave the link stuck. The only
		// recovery that PRESERVES the subscription is to drop the whole
		// connection: the monitor reconnects, the link re-attaches cleanly,
		// and a fresh link starts with full credit. Mirrors closeLink and
		// forceSettleRebuild. A durable receiver is kept alone on its own
		// session, so this teardown reaches no sibling link.
		if r.session != nil && failedConn != nil {
			r.session.notifyDisconnect(failedConn, err)
		}
		return
	}

	if link != nil {
		// Non-durable links CAN be safely full-closed and rebuilt in
		// isolation, so a link-scoped or transient fault rebuilds only THIS
		// link without touching the shared connection.
		closeCtx, cancel := context.WithTimeout(context.Background(), r.linkCloseTimeout())
		_ = link.Close(closeCtx)
		cancel()
	}

	if r.session != nil {
		// A link-scoped fault (e.g. *amqp.LinkError on a live
		// session) must rebuild only THIS link — escalating it via
		// notifyDisconnect would tear down the shared connection and
		// disrupt every other link on the session.
		if isLinkScopedError(err) {
			return
		}
		bridgeErr := MapError(err)
		if bridgeErr != nil && (bridgeErr.Code == shared.ErrCodeConnectionLost ||
			bridgeErr.Code == shared.ErrCodeUnavailable) {
			r.session.notifyDisconnect(failedConn, err)
		}
	}
}

func (r *Receiver) waitAndReconnect(ctx context.Context) error {
	r.mu.Lock()
	if r.link != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	if r.session == nil {
		return shared.ErrUnavailable.WithMessage("amqp10: no session")
	}

	events, unsub := r.session.Subscribe()
	defer unsub()

	if h := r.session.Health(ctx); h.Connected {
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace,
				"amqp10: receiver reconnect probe found session already connected",
				"address", redactURL(r.cfg.Address))
		}
	} else {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ev, ok := <-events:
				if !ok {
					return shared.ErrUnavailable.WithMessage("amqp10: session closed")
				}
				if ev.Type == ports.SessionConnected || ev.Type == ports.SessionReconciled {
					if logging.TraceEnabled(r.logger) {
						r.logger.Log(ctx, logging.LevelTrace,
							"amqp10: receiver reconnect signal received",
							"address", redactURL(r.cfg.Address),
							"event_type", ev.Type)
					}
					goto connected
				}
			}
		}
	}
connected:

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.link != nil {
		return nil
	}

	err := r.createLink(ctx)
	if err != nil {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp10: receiver link re-creation failed",
				"address", redactURL(r.cfg.Address), "error", err)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !shared.IsRecoverableError(err) {
			return err
		}
	} else if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "amqp10: receiver link re-created",
			"address", redactURL(r.cfg.Address))
	}
	return nil
}

// backoff implements exponential backoff with jitter.
type backoff struct {
	current time.Duration
}

const (
	backoffInitial    = 1 * time.Second
	backoffMax        = 30 * time.Second
	backoffMultiplier = 2
)

func newBackoff() *backoff {
	return &backoff{current: backoffInitial}
}

func (b *backoff) next() time.Duration {
	delay := b.current
	jitter := time.Duration(float64(delay) * 0.25 * (2*mathrand.Float64() - 1))
	delay += jitter

	b.current *= backoffMultiplier
	if b.current > backoffMax {
		b.current = backoffMax
	}

	return delay
}

func (b *backoff) reset() {
	b.current = backoffInitial
}
