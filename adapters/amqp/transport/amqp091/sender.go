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

	mu      sync.Mutex
	ch      *amqp.Channel
	conf    chan amqp.Confirmation
	returns chan amqp.Return
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

	sessionTag := domain.Tag{Key: domain.TagKeyEntity, Value: s.cfg.Exchange}

	s.mu.Lock()
	ch, err := s.ensureChannelLocked()
	if err != nil {
		s.mu.Unlock()
		return MapError(err)
	}
	confCh := s.conf
	returnsCh := s.returns

	// Defensive drain: under the deterministic ordering documented on
	// checkReturnedLocked every prior Send fully consumes its own
	// basic.return, so this loop should never find anything. If it
	// does, it indicates a code-path that bypassed checkReturnedLocked
	// (e.g., a publish path with Mandatory toggled at runtime); drop
	// any such residue rather than mis-attribute it to the next Send.
	if returnsCh != nil {
		for {
			select {
			case <-returnsCh:
				continue
			default:
			}
			break
		}
	}

	start := time.Now()
	if err := ch.PublishWithContext(sendCtx, exchange, routingKey, s.cfg.Mandatory, s.cfg.Immediate, pub); err != nil {
		s.resetChannelLocked()
		s.mu.Unlock()
		s.metrics.Timer(domain.MetricAMQP091PublishLatency, time.Since(start), sessionTag)
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp091: publish failed",
				"exchange", exchange, "routing_key", routingKey, "error", err)
		}
		return MapError(err)
	}

	confirmErr := s.waitConfirmLocked(sendCtx, confCh)
	if confirmErr == nil && s.cfg.Mandatory {
		confirmErr = s.checkReturnedLocked(returnsCh)
	}
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
		_ = ch.Close()
		return nil, MapError(err)
	}

	s.ch = ch
	s.conf = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	if s.cfg.Mandatory {
		s.returns = ch.NotifyReturn(make(chan amqp.Return, 1))
	} else {
		s.returns = nil
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelTrace,
			"amqp091: sender channel opened with confirms",
			"exchange", s.cfg.Exchange,
		)
	}

	return ch, nil
}

// checkReturnedLocked inspects the returns channel for a basic.return
// frame that the broker emits when Mandatory=true and the message was
// not routed to any queue.
//
// This is a NON-BLOCKING poll — no grace window — because the AMQP
// 0-9-1 spec and the underlying amqp091-go client together provide a
// hard ordering guarantee:
//
//  1. AMQP spec: for an unroutable mandatory publish the broker emits
//     basic.return BEFORE basic.ack on the wire (see
//     https://www.rabbitmq.com/publishers.html#unroutable).
//  2. amqp091-go (channel.go::dispatch): a SINGLE goroutine
//     demultiplexes incoming frames per channel. basicReturn is
//     handled by a synchronous send on the NotifyReturn listener
//     channel; basicAck is handled by a synchronous call to
//     confirms.One() which in turn does a synchronous send on the
//     NotifyPublish listener channel. The dispatcher processes
//     frames in network order, so by the time the caller observes
//     a confirm on confCh the matching return (if any) has already
//     been delivered to returnsCh.
//
// Send() serialises every publish under s.mu and uses returns/confirm
// channels with buffer size 1, so there can never be more than one
// in-flight publish on this channel — the buffer of 1 is sufficient
// and the dispatcher never blocks waiting for the listener.
//
// A previous implementation used a 50ms grace timer "in case the
// client reorders" — that was unnecessary defensive code that added
// 50ms per Send for every routable message and was simultaneously
// not strict enough for adversarial timing. The new contract is
// pinned by sender_mandatory_determinism_test.go.
//
// Caller must hold s.mu.
func (s *Sender) checkReturnedLocked(returnsCh chan amqp.Return) error {
	if returnsCh == nil {
		return nil
	}
	select {
	case ret, ok := <-returnsCh:
		if !ok {
			return nil
		}
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(context.Background(), logging.LevelDebug,
				"amqp091: mandatory message returned by broker",
				"reply_code", ret.ReplyCode,
				"reply_text", ret.ReplyText,
				"exchange", ret.Exchange,
				"routing_key", ret.RoutingKey,
			)
		}
		return domain.ErrNotFound.WithMessage(
			"amqp091: mandatory publish unroutable: " + ret.ReplyText)
	default:
		return nil
	}
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
		if logging.TraceEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelTrace, "amqp091: publish confirmed",
				"delivery_tag", confirmation.DeliveryTag,
			)
		}
		return nil
	}
}

// resetChannelLocked closes and clears the cached channel. Caller must hold s.mu.
func (s *Sender) resetChannelLocked() {
	if s.ch != nil {
		_ = s.ch.Close()
		s.ch = nil
		s.conf = nil
		s.returns = nil
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
