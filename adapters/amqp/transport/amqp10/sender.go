package amqp10

import (
	"context"
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
func (s *Sender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	// TODO(T03/T06): consume msg.Address as the AMQP 1.0 link target override.
	env := msg.Envelope
	sendCtx, cancel := s.applyTimeout(ctx)
	defer cancel()

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: sending",
			"address", redactURL(s.cfg.Address),
			"envelope_id", env.ID,
			"payload_len", len(env.Payload),
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
		s.handleSendFailure(ctx, link, linkConn, err)
		return MapError(err)
	}

	elapsed := s.clock().Since(start)
	s.metrics.Timer(shared.MetricAMQP10SendLatency, elapsed,
		shared.Tag{Key: shared.TagKeyEntity, Value: s.cfg.Address})

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: send complete",
			"address", redactURL(s.cfg.Address),
			"envelope_id", env.ID,
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

// SendBatch sends multiple envelopes individually over the AMQP 1.0 link.
func (s *Sender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) (int, error) {
	if err := s.ensureLink(ctx); err != nil {
		return 0, err
	}

	var sent int
	for _, m := range msgs {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		if err := s.Send(ctx, m); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
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
	sess := s.session.AMQPSession()
	if sess == nil {
		return shared.ErrUnavailable.WithMessage("amqp10: session not connected")
	}
	conn := s.session.Conn()

	sender, err := sess.NewSenderLink(
		ctx,
		s.cfg.Address,
		s.cfg.DurabilityMode,
		s.cfg.Routing.capability(),
	)
	if err != nil {
		return MapError(err)
	}

	s.link = sender
	s.linkConn = conn

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: sender link created",
			"address", s.cfg.Address)
	}

	return nil
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
// errors (connection lost or unclassified unavailable).
func (s *Sender) notifySessionIfConnectionLost(failedConn amqpConn, err error) {
	if s.session == nil {
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
	link := s.link
	s.link = nil
	s.linkConn = nil
	s.mu.Unlock()

	if link != nil {
		if err := link.Close(ctx); err != nil {
			return MapError(err)
		}
	}
	return nil
}
