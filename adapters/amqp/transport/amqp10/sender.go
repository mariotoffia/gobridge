package amqp10

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.Sender      = (*Sender)(nil)
	_ ports.BatchSender = (*Sender)(nil)
)

// Sender implements ports.Sender and ports.BatchSender for AMQP 1.0 links.
type Sender struct {
	cfg     SenderConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter

	mu   sync.Mutex
	link *amqp.Sender
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
	return &Sender{
		cfg:     cfg,
		session: session,
		logger:  l,
		metrics: m,
	}, nil
}

// Send publishes a single envelope to the AMQP 1.0 broker. The entire
// ensure-link + send cycle is serialized under the mutex to prevent
// use-after-close races when handleLinkError clears the link concurrently.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	msg := s.buildMessage(env)

	sendCtx, cancel := s.applyTimeout(ctx)
	defer cancel()

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: sending",
			"address", s.cfg.Address,
			"envelope_id", env.ID,
			"payload_len", len(env.Payload),
		)
	}

	s.mu.Lock()
	if s.link == nil {
		if err := s.createLink(ctx); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	link := s.link
	s.mu.Unlock()

	start := time.Now()

	if err := link.Send(sendCtx, msg, nil); err != nil {
		s.handleLinkError(err)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp10: send failed",
				"address", s.cfg.Address, "error", err)
		}
		return MapError(err)
	}

	elapsed := time.Since(start)
	s.metrics.Timer(domain.MetricAMQP10SendLatency, elapsed,
		domain.Tag{Key: domain.TagKeyEntity, Value: s.cfg.Address})

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: send complete",
			"address", s.cfg.Address,
			"envelope_id", env.ID,
			"duration", elapsed,
		)
	}

	return nil
}

// SendBatch sends multiple envelopes individually over the AMQP 1.0 link.
// Returns the count of successfully sent messages.
func (s *Sender) SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error) {
	if err := s.ensureLink(ctx); err != nil {
		return 0, err
	}

	var sent int
	for _, env := range envs {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		if err := s.Send(ctx, env); err != nil {
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
		return domain.ErrUnavailable.WithMessage("amqp10: session not connected")
	}

	opts := &amqp.SenderOptions{
		TargetCapabilities: []string{s.cfg.Routing.capability()},
	}
	if s.cfg.DurabilityMode > 0 {
		opts.Durability = amqp.Durability(s.cfg.DurabilityMode)
	}

	sender, err := sess.NewSender(ctx, s.cfg.Address, opts)
	if err != nil {
		return MapError(err)
	}

	s.link = sender

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

func (s *Sender) buildMessage(env *domain.Envelope) *amqp.Message {
	msg := headersToMessage(env.Headers)

	msg.Data = [][]byte{env.Payload}

	if msg.Properties == nil {
		msg.Properties = &amqp.MessageProperties{}
	}

	if env.ID != "" {
		msg.Properties.MessageID = env.ID
	}
	if env.Subject != "" {
		msg.Properties.Subject = &env.Subject
	}
	if env.HasExpiry() {
		expiry := env.ExpiresAt
		msg.Properties.AbsoluteExpiryTime = &expiry
	}

	if !env.CreatedAt.IsZero() {
		msg.Properties.CreationTime = &env.CreatedAt
	}

	return msg
}

func (s *Sender) handleLinkError(err error) {
	s.mu.Lock()
	link := s.link
	s.link = nil
	s.mu.Unlock()

	if link != nil {
		_ = link.Close(context.Background())
	}

	if s.session != nil {
		bridgeErr := MapError(err)
		if bridgeErr != nil && (bridgeErr.Code == domain.ErrCodeConnectionLost ||
			bridgeErr.Code == domain.ErrCodeUnavailable) {
			conn := s.session.Conn()
			s.session.notifyDisconnect(conn, err)
		}
	}
}

// Close closes the sender link.
func (s *Sender) Close(ctx context.Context) error {
	s.mu.Lock()
	link := s.link
	s.link = nil
	s.mu.Unlock()

	if link != nil {
		return link.Close(ctx)
	}
	return nil
}
