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
//
// The mutex protects link CREATION and detach only; it is released
// before the network-blocking link.Send so concurrent callers achieve
// real parallelism. *amqp.Sender.Send is documented as safe for
// concurrent use, and we coalesce concurrent failure handling so the
// link is detached and asynchronously closed exactly once per failure
// cycle.
type Sender struct {
	cfg     SenderConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter

	mu sync.Mutex
	// link is the active publish link; nil when no link has been created
	// yet or after a failure has detached it.
	link amqpSenderLink
	// linkConn captures WHICH session connection the current link was
	// created on. handleSendFailure passes this (not the session's
	// CURRENT connection) to notifyDisconnect so a stale link error
	// surfacing AFTER the session has already reconnected to a new
	// connection cannot incorrectly tear down the new connection.
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
	return &Sender{
		cfg:     cfg,
		session: session,
		logger:  l,
		metrics: m,
	}, nil
}

// Send publishes a single envelope to the AMQP 1.0 broker.
//
// Concurrency model:
//   - Link creation is serialised under s.mu so concurrent first-time
//     callers cannot race two NewSender calls.
//   - The mutex is RELEASED before invoking link.Send. *amqp.Sender.Send
//     is safe for concurrent use, so many goroutines may publish on the
//     same link at the same time and achieve genuine parallelism.
//   - On error, only the goroutine that observes s.link still pointing
//     at the failed link performs the detach+async-close; losers of the
//     race simply return the mapped error, so the link is closed at
//     most once per failure cycle.
//   - The async close (5 s timeout) runs in a goroutine so a slow
//     broker shutdown never blocks subsequent Send/Close calls.
func (s *Sender) Send(ctx context.Context, env *domain.Envelope) error {
	msg := s.buildMessage(env)

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
		// Link creation honours the per-send timeout so a hung broker
		// during NewSender cannot block longer than s.cfg.Timeout.
		if err := s.createLink(sendCtx); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	link := s.link
	linkConn := s.linkConn
	s.mu.Unlock()

	start := time.Now()

	err := link.Send(sendCtx, msg, nil)
	if err != nil {
		s.handleSendFailure(ctx, link, linkConn, err)
		return MapError(err)
	}

	elapsed := time.Since(start)
	s.metrics.Timer(domain.MetricAMQP10SendLatency, elapsed,
		domain.Tag{Key: domain.TagKeyEntity, Value: s.cfg.Address})

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: send complete",
			"address", redactURL(s.cfg.Address),
			"envelope_id", env.ID,
			"duration", elapsed,
		)
	}

	return nil
}

// handleSendFailure coalesces concurrent failure handling. Only the
// goroutine that observes s.link still pointing at the failed link
// performs the detach+close+session-notify; concurrent Send failures
// against the same link produce a single close.
//
// failedConn is the connection the failed link was created on. It is
// passed verbatim to notifyDisconnect so the session can ignore stale
// notifications: if the session has already reconnected to a new
// connection, failedConn != s.session.Conn() and notifyDisconnect is
// a no-op — preventing destruction of the freshly-reconnected
// connection by an in-flight Send error from a previous lifecycle.
func (s *Sender) handleSendFailure(ctx context.Context, failed amqpSenderLink, failedConn amqpConn, err error) {
	s.mu.Lock()
	weDetach := s.link == failed
	if weDetach {
		s.link = nil
		s.linkConn = nil
	}
	s.mu.Unlock()

	if weDetach {
		closeLinkAsync(failed)
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
	// Capture the connection THIS link is being created on so a later
	// failure carries the right identity to notifyDisconnect, even if
	// the session has reconnected in the meantime.
	conn := s.session.Conn()

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

// closeLinkAsync closes a detached AMQP sender link off the hot path so
// that a slow broker shutdown does not block other senders or callers.
// The 5-second close timeout matches the previous synchronous behaviour.
func closeLinkAsync(link amqpSenderLink) {
	if link == nil {
		return
	}
	go func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = link.Close(closeCtx)
	}()
}

// notifySessionIfConnectionLost signals the session for connection-level
// errors (connection lost or unclassified unavailable). Business-logic
// errors like ErrCodeInvalidPayload are NOT escalated.
//
// failedConn is the connection the failed link belonged to (captured at
// link creation). Passing it — instead of s.session.Conn() — lets the
// session ignore this notification when it has already reconnected to
// a different connection, which protects the new connection from being
// torn down by a stale Send error from the previous lifecycle.
func (s *Sender) notifySessionIfConnectionLost(failedConn amqpConn, err error) {
	if s.session == nil {
		return
	}
	bridgeErr := MapError(err)
	if bridgeErr != nil && (bridgeErr.Code == domain.ErrCodeConnectionLost ||
		bridgeErr.Code == domain.ErrCodeUnavailable) {
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
		return link.Close(ctx)
	}
	return nil
}
