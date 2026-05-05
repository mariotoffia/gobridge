package amqp10

import (
	"context"
	"log/slog"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
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
	link     *receiverLink
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

	r.startedOnce.Do(func() { close(r.started) })

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
	conn := r.session.Conn()

	recv, err := sess.NewReceiverLink(
		ctx,
		r.cfg.Address,
		int32(r.cfg.LinkCredit),
		r.cfg.DurabilityMode,
		r.cfg.Routing.capability(),
	)
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

		start := r.clock().Now()
		del, err := link.Receive(ctx, r.cfg.Address, r.logger, r.metrics, r.clock())
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
			case <-r.clock().After(delay):
			}
			continue
		}

		r.metrics.Timer(domain.MetricAMQP10ReceiveLatency, r.clock().Since(start),
			domain.Tag{Key: domain.TagKeyEntity, Value: r.cfg.Address})
		backoff.reset()

		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "amqp10: received message",
				"address", redactURL(r.cfg.Address),
				"message_id", del.Envelope().ID,
				"body_len", len(del.Envelope().Payload),
			)
		}

		if err := emit(ctx, del); err != nil {
			return err
		}
	}
}

// handleLinkError detaches the receiver's link after a non-transient
// failure, forwarding connection-level errors to the session via the
// connection identity captured at link-creation time.
func (r *Receiver) handleLinkError(err error) {
	r.mu.Lock()
	link := r.link
	r.link = nil
	failedConn := r.linkConn
	r.linkConn = nil
	r.mu.Unlock()

	if link != nil {
		timeout := 5 * time.Second
		if r.session != nil {
			timeout = r.session.opts.LinkCloseTimeout
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), timeout)
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

	if r.session == nil {
		return domain.ErrUnavailable.WithMessage("amqp10: no session")
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
					return domain.ErrUnavailable.WithMessage("amqp10: session closed")
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
