package amqp091

import (
	"context"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for AMQP 0-9-1.
// It publishes messages with publisher confirms enabled.
type Sender struct {
	cfg     SenderConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter

	mu   sync.Mutex
	ch   *amqp.Channel
	conf chan amqp.Confirmation
}

// NewSender creates a Sender bound to the given Session.
func NewSender(cfg SenderConfig) *Sender {
	cfg.applyDefaults()
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
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
	}
}

// Send publishes a single envelope to the AMQP broker with publisher
// confirms. The exchange and routing key are derived from configuration
// and the envelope's Subject. The entire publish+confirm cycle is
// serialized to prevent confirm-channel races under concurrent callers.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	pub := envelopeToPublishing(env, s.cfg)
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

	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: s.cfg.Exchange}

	s.mu.Lock()
	ch, err := s.ensureChannelLocked()
	if err != nil {
		s.mu.Unlock()
		return MapError(err)
	}
	confCh := s.conf

	start := time.Now()
	if err := ch.PublishWithContext(sendCtx, exchange, routingKey, s.cfg.Mandatory, s.cfg.Immediate, pub); err != nil {
		s.resetChannelLocked()
		s.mu.Unlock()
		s.metrics.Timer(domain.MetricAMQP091PublishLatency, time.Since(start), sessionTag)
		logging.DebugContext(s.logger, ctx, "amqp091: publish failed",
			"exchange", exchange, "routing_key", routingKey, "error", err)
		return MapError(err)
	}

	confirmErr := s.waitConfirmLocked(sendCtx, confCh)
	s.mu.Unlock()

	elapsed := time.Since(start)
	s.metrics.Timer(domain.MetricAMQP091PublishLatency, elapsed, sessionTag)

	if confirmErr != nil {
		return confirmErr
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

// SendBatch publishes multiple envelopes sequentially with publisher
// confirms. Returns the number of successfully published messages.
func (s *Sender) SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error) {
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
func (s *Sender) ensureChannelLocked() (*amqp.Channel, error) {
	if s.ch != nil {
		return s.ch, nil
	}

	if s.session == nil {
		return nil, domain.ErrUnavailable.WithMessage("amqp091: no session")
	}
	conn := s.session.Connection()
	if conn == nil {
		return nil, domain.ErrUnavailable.WithMessage("amqp091: session not connected")
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, MapError(err)
	}

	if err := ch.Confirm(false); err != nil {
		ch.Close()
		return nil, MapError(err)
	}

	s.ch = ch
	s.conf = ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	return ch, nil
}

// waitConfirmLocked waits for the publish confirmation. Caller must hold s.mu.
func (s *Sender) waitConfirmLocked(ctx context.Context, confCh chan amqp.Confirmation) error {
	if confCh == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		s.resetChannelLocked()
		return MapError(ctx.Err())
	case confirmation, ok := <-confCh:
		if !ok {
			s.resetChannelLocked()
			return domain.ErrUnavailable.WithMessage("amqp091: confirm channel closed")
		}
		if !confirmation.Ack {
			return domain.ErrUnavailable.WithMessage("amqp091: publish nacked by broker")
		}
		return nil
	}
}

// resetChannelLocked closes and clears the cached channel. Caller must hold s.mu.
func (s *Sender) resetChannelLocked() {
	if s.ch != nil {
		s.ch.Close()
		s.ch = nil
		s.conf = nil
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
