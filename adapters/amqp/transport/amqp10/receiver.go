package amqp10

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for AMQP 1.0 links.
// It creates a receiver link on the session's AMQP session and loops
// calling Receive, converting messages to deliveries.
type Receiver struct {
	cfg     ReceiverConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter

	mu   sync.Mutex
	link *amqp.Receiver
	// linkConn captures WHICH session connection the current link was
	// created on. handleLinkError passes this (not the session's CURRENT
	// connection) to notifyDisconnect so a stale link error surfacing
	// AFTER the session has already reconnected to a new connection
	// cannot incorrectly tear down the new connection.
	linkConn amqpConn
}

// NewReceiver creates an AMQP 1.0 Receiver.
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
	return &Receiver{
		cfg:     cfg,
		session: session,
		logger:  l,
		metrics: m,
	}, nil
}

// Run creates a receiver link and enters the receive loop. It blocks until
// the context is cancelled or emit returns an error.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "amqp10: receiver starting",
			"address", redactURL(r.cfg.Address),
			"link_credit", r.cfg.LinkCredit,
		)
	}

	if err := r.ensureLink(ctx); err != nil {
		return err
	}
	defer r.closeLink()

	return r.receiveLoop(ctx, emit)
}

func (r *Receiver) closeLink() {
	r.mu.Lock()
	link := r.link
	r.link = nil
	r.linkConn = nil
	r.mu.Unlock()

	if link != nil {
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(context.Background(), logging.LevelTrace, "amqp10: closing receiver link",
				"address", redactURL(r.cfg.Address))
		}
		_ = link.Close(context.Background())
	}
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
	sess := r.session.AMQPSession()
	if sess == nil {
		return domain.ErrUnavailable.WithMessage("amqp10: session not connected")
	}
	// Capture the connection THIS link is being created on so a later
	// failure carries the right identity to notifyDisconnect, even if
	// the session has reconnected in the meantime (see handleLinkError).
	conn := r.session.Conn()

	opts := &amqp.ReceiverOptions{
		Credit:             int32(r.cfg.LinkCredit),
		SourceCapabilities: []string{r.cfg.Routing.capability()},
	}
	if r.cfg.DurabilityMode > 0 {
		opts.Durability = amqp.Durability(r.cfg.DurabilityMode)
	}

	recv, err := sess.NewReceiver(ctx, r.cfg.Address, opts)
	if err != nil {
		return MapError(err)
	}

	r.link = recv
	r.linkConn = conn
	return nil
}

func (r *Receiver) receiveLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	backoff := newBackoff()

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
			continue
		}

		start := time.Now()
		msg, err := link.Receive(ctx, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			bridgeErr := MapError(err)
			if bridgeErr != nil && bridgeErr.Class != domain.ErrorTransient {
				r.handleLinkError(err)
				return bridgeErr
			}

			delay := backoff.next()
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
			case <-time.After(delay):
			}
			continue
		}

		r.metrics.Timer(domain.MetricAMQP10ReceiveLatency, time.Since(start),
			domain.Tag{Key: domain.TagKeyEntity, Value: r.cfg.Address})
		backoff.reset()

		del := r.convertMessage(ctx, msg, link)

		if err := emit(ctx, del); err != nil {
			return err
		}
	}
}

func (r *Receiver) convertMessage(ctx context.Context, msg *amqp.Message, link *amqp.Receiver) *Delivery {
	headers := messageToHeaders(msg)

	var msgID string
	if msg.Properties != nil && msg.Properties.MessageID != nil {
		if s, ok := msg.Properties.MessageID.(string); ok {
			msgID = s
		}
	}

	subject := r.cfg.Address
	if msg.Properties != nil && msg.Properties.Subject != nil {
		subject = *msg.Properties.Subject
	}

	var body []byte
	if len(msg.Data) > 0 {
		body = msg.Data[0]
	} else if msg.Value != nil {
		if b, ok := msg.Value.([]byte); ok {
			body = b
		}
	}

	if msgID == "" {
		msgID = generateEnvelopeID()
	}

	env := &domain.Envelope{
		ID:        msgID,
		Subject:   subject,
		Payload:   body,
		Headers:   headers,
		CreatedAt: time.Now(),
	}

	if msg.Properties != nil && msg.Properties.AbsoluteExpiryTime != nil {
		env.ExpiresAt = *msg.Properties.AbsoluteExpiryTime
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "amqp10: received message",
			"address", redactURL(r.cfg.Address),
			"message_id", msgID,
			"body_len", len(body),
		)
	}

	return NewDelivery(env, msg, link, r.logger, r.metrics)
}

// handleLinkError detaches the receiver's link after a non-transient
// failure. When the failure is connection-level it forwards a disconnect
// notification to the session, identifying the failed connection by the
// pointer captured at link-creation time (NOT the session's CURRENT
// connection). This ensures a stale link error from the previous
// connection lifecycle cannot tear down a freshly-reconnected
// connection — the session's stale-pointer guard would otherwise match
// the current connection and incorrectly destroy it.
func (r *Receiver) handleLinkError(err error) {
	r.mu.Lock()
	link := r.link
	r.link = nil
	failedConn := r.linkConn
	r.linkConn = nil
	r.mu.Unlock()

	if link != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = link.Close(closeCtx)
		closeCancel()
	}

	if r.session != nil {
		bridgeErr := MapError(err)
		if bridgeErr != nil && (bridgeErr.Code == domain.ErrCodeConnectionLost ||
			bridgeErr.Code == domain.ErrCodeUnavailable) {
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

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1 * time.Second):
	}

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
		if !domain.IsRecoverableError(err) {
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

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp10: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
