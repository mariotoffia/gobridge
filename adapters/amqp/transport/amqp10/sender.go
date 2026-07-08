package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// senderLinkAPI is the link-level operation surface the Sender depends
// on. It is satisfied by *senderLink (the production wrapper around
// *amqp.Sender) and may also be satisfied by test doubles.
type senderLinkAPI interface {
	SendEnvelope(ctx context.Context, env *messaging.Envelope) error
	Close(ctx context.Context) error
}

// Sender implements ports.Sender and ports.BatchSender for AMQP 1.0 links.
//
// The mutex protects link CREATION and detach only; it is released
// before the network-blocking link.SendEnvelope so concurrent callers
// achieve real parallelism.
type Sender struct {
	cfg     SenderConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu       sync.Mutex
	link     senderLinkAPI
	linkConn amqpConn
	// closed is set by Close under mu. Once closed, createLink refuses to
	// re-attach a link (and thus re-register the sender in session
	// health), so a late Send/SendBatch returns a permanent
	// transport-closed error instead of silently resurrecting the sender
	// (finding F14).
	closed bool

	// attach creates a new sender link on the current session connection.
	// It defaults to defaultAttach (session-backed); tests may override it
	// to exercise the attach path — notably the SendBatch attach timeout
	// (finding 4) — without a live broker.
	attach func(ctx context.Context) (senderLinkAPI, amqpConn, error)
}

// NewSender creates an AMQP 1.0 Sender.
func NewSender(cfg SenderConfig, session *Session) (*Sender, error) {
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
	return &Sender{
		cfg:     cfg,
		session: session,
		logger:  l,
		metrics: m,
		clk:     clk,
	}, nil
}

func (s *Sender) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Send publishes a single envelope to the AMQP 1.0 broker.
//
// Address validation: AMQP 1.0 sender links are address-bound. When
// msg.Address is empty, the configured s.cfg.Address is used. A
// non-empty msg.Address must match s.cfg.Address exactly; any other
// value is rejected with shared.ErrInvalidTopic without contacting the
// broker. (Dynamic per-address link creation is a possible future
// extension.) The logical Envelope.Subject is mapped to
// Properties.Subject by envelopeToMessage and never participates in
// link routing.
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	if env == nil {
		return shared.ErrInvalidPayload.WithMessage("amqp10: nil envelope")
	}
	if msg.Address != "" && msg.Address != s.cfg.Address {
		return shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
			"amqp10: address %q does not match configured sender link address %q",
			msg.Address, s.cfg.Address))
	}
	sendCtx, cancel := s.applyTimeout(ctx)
	defer cancel()

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: sending",
			"address", redactURL(s.cfg.Address),
			"envelope_id", env.ID(),
			"payload_len", len(env.Payload()),
		)
	}

	s.mu.Lock()
	if s.link == nil {
		if err := s.createLink(sendCtx); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	link := s.link
	linkConn := s.linkConn
	s.mu.Unlock()

	start := s.clock().Now()

	err := link.SendEnvelope(sendCtx, env)
	if err != nil {
		// errNotAccepted marks a broker disposition failure (Released,
		// Modified, Rejected): the transfer itself succeeded and the
		// LINK IS HEALTHY — detaching it (and possibly escalating to a
		// connection teardown) would punish the transport for a
		// per-message outcome. Return the classified error and keep the
		// link for the next attempt.
		if !errors.Is(err, errNotAccepted) {
			s.handleSendFailure(ctx, link, linkConn, err)
		}
		return MapError(err)
	}

	elapsed := s.clock().Since(start)
	s.metrics.Timer(MetricAMQP10SendLatency, elapsed,
		shared.Tag{Key: shared.TagKeyEntity, Value: s.cfg.Address})

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: send complete",
			"address", redactURL(s.cfg.Address),
			"envelope_id", env.ID(),
			"duration", elapsed,
		)
	}

	return nil
}

// handleSendFailure coalesces concurrent failure handling so the link is
// detached and asynchronously closed at most once per failure cycle.
func (s *Sender) handleSendFailure(ctx context.Context, failed senderLinkAPI, failedConn amqpConn, err error) {
	s.mu.Lock()
	weDetach := s.link == failed
	if weDetach {
		s.link = nil
		s.linkConn = nil
	}
	s.mu.Unlock()

	if weDetach {
		timeout := 5 * time.Second
		if s.session != nil {
			timeout = s.session.opts.LinkCloseTimeout
		}
		closeLinkAsync(failed, timeout)
		// The link is down until Send re-establishes it: reflect that in
		// Session.Health and record the cause (finding 9).
		if s.session != nil {
			s.session.markSenderLink(s, false)
			s.session.noteLinkError(err)
		}
		s.notifySessionIfConnectionLost(failedConn, err)
	}
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: send failed",
			"address", redactURL(s.cfg.Address),
			"error", err,
			"detached_link", weDetach,
		)
	}
}

// SendBatch sends each envelope individually over the AMQP 1.0 link.
// The whole batch is pre-validated up front (non-nil envelope, address
// either empty or equal to s.cfg.Address); any violation — or a link
// setup failure — rejects the entire batch with (nil, err) before any
// message is dispatched. Once dispatched, the returned slice is
// index-aligned with msgs and each entry carries that message's
// outcome (nil Err on success); see ports.BatchSender for the contract.
func (s *Sender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) ([]ports.BatchResult, error) {
	for _, m := range msgs {
		if m.Envelope == nil {
			return nil, shared.ErrInvalidPayload.WithMessage("amqp10: nil envelope")
		}
		if m.Address != "" && m.Address != s.cfg.Address {
			return nil, shared.ErrInvalidTopic.WithMessage(fmt.Sprintf(
				"amqp10: address %q does not match configured sender link address %q",
				m.Address, s.cfg.Address))
		}
	}

	// Bound the initial link attach the same way Send bounds its own
	// attach: without this, a broker that accepts TCP but never answers
	// the attach hangs the batch caller indefinitely (finding 4). Only
	// the attach is bounded here — each per-message Send below applies
	// its own cfg.Timeout.
	attachCtx, cancel := s.applyTimeout(ctx)
	err := s.ensureLink(attachCtx)
	cancel()
	if err != nil {
		return nil, err
	}

	results := make([]ports.BatchResult, len(msgs))
	for i, m := range msgs {
		results[i] = ports.BatchResult{Index: i, Err: s.Send(ctx, m)}
	}
	return results, nil
}

func (s *Sender) ensureLink(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.link != nil {
		return nil
	}

	return s.createLink(ctx)
}

func (s *Sender) createLink(ctx context.Context) error {
	// F14: createLink is the single choke point for link attach (called
	// under s.mu from Send and ensureLink). Refuse once closed so a late
	// Send cannot re-attach a link and re-enter session health.
	if s.closed {
		return shared.ErrUnavailable.
			WithMessage("amqp10: sender closed").
			Wrap(shared.ErrTransportClosedPermanently)
	}

	attach := s.attach
	if attach == nil {
		attach = s.defaultAttach
	}
	link, conn, err := attach(ctx)
	if err != nil {
		return err
	}

	s.link = link
	s.linkConn = conn

	// Register this sender's link for health reporting: a subsequent
	// failure marks it down and degrades Session.Health (finding 9).
	if s.session != nil {
		s.session.registerSender(s)
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: sender link created",
			"address", s.cfg.Address)
	}

	return nil
}

// defaultAttach is the production link-attach: it captures the
// (session, conn) pair atomically so the link is never associated with a
// stale connection identity (see Session.sessionAndConn) and opens a new
// sender link on it.
//
// (ireturn allow-list category 5): defaultAttach must return the
// senderLinkAPI interface so unit tests can inject a fake attach that
// exercises the SendBatch attach timeout (finding 4) without a broker.
//
//nolint:ireturn // senderLinkAPI is an adapter-internal mock seam
func (s *Sender) defaultAttach(ctx context.Context) (senderLinkAPI, amqpConn, error) {
	sess, conn := s.session.sessionAndConn()
	if sess == nil {
		return nil, nil, shared.ErrUnavailable.WithMessage("amqp10: session not connected")
	}

	sender, err := sess.NewSenderLink(
		ctx,
		s.cfg.Address,
		s.cfg.DurabilityMode,
		s.cfg.Routing.capability(),
		s.cfg.durable(),
	)
	if err != nil {
		return nil, nil, MapError(err)
	}
	return sender, conn, nil
}

func (s *Sender) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, s.cfg.Timeout)
	}
	return ctx, func() {}
}

// closeLinkAsync closes a detached AMQP sender link off the hot path so
// that a slow broker shutdown does not block other senders or callers.
func closeLinkAsync(link senderLinkAPI, timeout time.Duration) {
	if link == nil {
		return
	}
	go func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), timeout)
		defer closeCancel()
		_ = link.Close(closeCtx)
	}()
}

// notifySessionIfConnectionLost signals the session for connection-level
// errors (connection lost or unclassified unavailable). Link-scoped
// faults (*amqp.LinkError on a live session) are excluded: only this
// sender's link needs rebuilding, not the shared connection.
func (s *Sender) notifySessionIfConnectionLost(failedConn amqpConn, err error) {
	if s.session == nil {
		return
	}
	if isLinkScopedError(err) {
		return
	}
	bridgeErr := MapError(err)
	if bridgeErr != nil && (bridgeErr.Code == shared.ErrCodeConnectionLost ||
		bridgeErr.Code == shared.ErrCodeUnavailable) {
		s.session.notifyDisconnect(failedConn, err)
	}
}

// Close closes the sender link.
func (s *Sender) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	link := s.link
	s.link = nil
	s.linkConn = nil
	s.mu.Unlock()

	if s.session != nil {
		s.session.unregisterSender(s)
	}

	if link != nil {
		if err := link.Close(ctx); err != nil {
			return MapError(err)
		}
	}
	return nil
}
