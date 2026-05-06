package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"sync"

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
	sc *senderChannel
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
	}
}

func (s *Sender) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Send publishes a single envelope to the AMQP broker with publisher
// confirms. The exchange and routing key are derived from configuration
// and the envelope's Subject. The entire publish+confirm cycle is
// serialized to prevent confirm-channel races under concurrent callers.
func (s *Sender) Send(ctx context.Context, env *messaging.Envelope) error {
	exchange := s.cfg.Exchange
	routingKey := s.cfg.RoutingKey
	if routingKey == "" {
		routingKey = env.Subject
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp091: publishing",
			"exchange", exchange,
			"routing_key", routingKey,
			"envelope_id", env.ID,
			"payload_len", len(env.Payload),
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
	res, perr := sc.PublishConfirmed(sendCtx, exchange, routingKey,
		s.cfg.Mandatory, s.cfg.Immediate, env, s.cfg, s.clock())
	if perr != nil {
		s.resetChannelLocked()
		s.mu.Unlock()
		s.metrics.Timer(shared.MetricAMQP091PublishLatency, s.clock().Since(start), sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp091: publish failed",
				"exchange", exchange, "routing_key", routingKey, "error", perr)
		}
		return mapPublishError(perr)
	}
	s.mu.Unlock()

	elapsed := s.clock().Since(start)
	s.metrics.Timer(shared.MetricAMQP091PublishLatency, elapsed, sessionTag)

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
			"envelope_id", env.ID,
			"duration", elapsed,
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

// SendBatch publishes multiple envelopes sequentially with publisher
// confirms. Returns the number of successfully published messages.
func (s *Sender) SendBatch(ctx context.Context, envs []*messaging.Envelope) (int, error) {
	sent := 0
	for _, env := range envs {
		if err := s.Send(ctx, env); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// ensureChannelLocked opens a channel if none is cached. Caller must hold s.mu.
func (s *Sender) ensureChannelLocked() (*senderChannel, error) {
	if s.sc != nil {
		return s.sc, nil
	}
	if s.session == nil {
		return nil, shared.ErrUnavailable.WithMessage("amqp091: no session")
	}
	conn := s.session.Connection()
	if conn == nil {
		return nil, shared.ErrUnavailable.WithMessage("amqp091: session not connected")
	}

	sc, err := openSenderChannel(conn, s.cfg.Mandatory)
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
	return sc, nil
}

// resetChannelLocked closes and clears the cached channel. Caller must hold s.mu.
func (s *Sender) resetChannelLocked() {
	if s.sc != nil {
		_ = s.sc.Close()
		s.sc = nil
	}
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
