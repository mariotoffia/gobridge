package amqp091

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for AMQP 0-9-1. It consumes
// messages from a queue via the Session's connection and emits
// each message as a ports.Delivery.
type Receiver struct {
	cfg         ReceiverConfig
	session     *Session
	logger      *slog.Logger
	metrics     ports.MetricsExporter
	clk         clock.Clock
	started     chan struct{}
	startedOnce sync.Once
}

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(cfg ReceiverConfig) *Receiver {
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
	return &Receiver{
		cfg:     cfg,
		session: cfg.Session,
		logger:  l,
		metrics: m,
		clk:     clk,
		started: make(chan struct{}),
	}
}

func (r *Receiver) clock() clock.Clock {
	if r.clk != nil {
		return r.clk
	}
	return clock.System
}

// Started returns a channel that is closed once the receiver's
// channel and consumer have been set up and the consume loop is live.
// It satisfies ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// Run starts consuming messages from the configured queue. It blocks
// until ctx is cancelled or an unrecoverable error occurs. On channel
// or connection errors, it waits for the session to reconnect and
// re-establishes the consumer.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "amqp091: receiver starting",
			"queue", r.cfg.QueueName,
			"consumer_tag", r.cfg.ConsumerTag,
		)
	}

	for {
		err := r.consumeLoop(ctx, emit)
		if err == nil || ctx.Err() != nil {
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "amqp091: receiver stopped",
					"queue", r.cfg.QueueName, "reason", "context_cancelled")
			}
			return ctx.Err()
		}

		if r.isEmitError(err) {
			return errors.Unwrap(err)
		}

		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp091: consumer channel lost, waiting for reconnect",
				"queue", r.cfg.QueueName, "error", err)
		}

		if !r.waitForReconnect(ctx) {
			return ctx.Err()
		}
	}
}

// emitError wraps errors from the emit callback so they can be
// distinguished from transport-layer errors.
type emitError struct{ err error }

func (e *emitError) Error() string { return e.err.Error() }
func (e *emitError) Unwrap() error { return e.err }

// isEmitError returns true when the error originated from the emit callback
// rather than from the AMQP transport layer.
func (r *Receiver) isEmitError(err error) bool {
	var ee *emitError
	return errors.As(err, &ee)
}

func (r *Receiver) consumeLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	ch, err := r.openChannel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	if r.cfg.PrefetchCount > 0 || r.cfg.PrefetchSize > 0 {
		if err := ch.Qos(r.cfg.PrefetchCount, r.cfg.PrefetchSize, false); err != nil {
			return MapError(err)
		}
	}

	// Generate a fresh tag per consume attempt. Even when the user
	// supplies a base tag, append a unique suffix so that reconnecting
	// after a connection drop never collides with the broker's stale
	// view of the previous consumer (RabbitMQ only frees a tag once it
	// detects the prior connection is dead, which can lag the client).
	consumerTag := generateConsumerTag()
	if r.cfg.ConsumerTag != "" {
		consumerTag = r.cfg.ConsumerTag + "-" + consumerTag
	}

	consumeStart := r.clock().Now()
	deliveries, err := ch.Consume(
		r.cfg.QueueName,
		consumerTag,
		r.cfg.AutoAck,
		r.cfg.Exclusive,
		false, // noLocal: RabbitMQ does not support this
		false, // noWait
		nil,
	)
	if err != nil {
		return MapError(err)
	}
	r.metrics.Timer(domain.MetricAMQP091ConsumeLatency, r.clock().Since(consumeStart),
		domain.Tag{Key: domain.TagKeyEntity, Value: r.cfg.QueueName})

	r.startedOnce.Do(func() { close(r.started) })

	chanClose := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		// Priority check: if the caller has cancelled the context, return
		// before the normal multi-way select picks a delivery or
		// channel-close event randomly. Without this, the runtime's
		// fair scheduling of select cases would let the loop process
		// pending deliveries even after cancellation, defeating
		// graceful shutdown under load.
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case amqpErr := <-chanClose:
			if amqpErr != nil {
				return MapError(amqpErr)
			}
			return domain.ErrConnectionLost.WithMessage("amqp091: channel closed by broker")
		case d, ok := <-deliveries:
			if !ok {
				return domain.ErrConnectionLost.WithMessage("amqp091: delivery channel closed")
			}
			if err := r.handleDelivery(ctx, d, emit); err != nil {
				return err
			}
		}
	}
}

func (r *Receiver) handleDelivery(ctx context.Context, d amqp.Delivery, emit func(context.Context, ports.Delivery) error) error {
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "amqp091: message received",
			"queue", r.cfg.QueueName,
			"delivery_tag", d.DeliveryTag,
			"routing_key", d.RoutingKey,
			"payload_len", len(d.Body),
		)
	}

	env := deliveryToEnvelope(d, r.clock())
	del := NewDelivery(env, d, r.logger, r.metrics, r.clock())

	if err := emit(ctx, del); err != nil {
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "amqp091: emit error",
				"queue", r.cfg.QueueName, "error", err)
		}
		return &emitError{err: err}
	}
	return nil
}

func (r *Receiver) openChannel() (*amqp.Channel, error) {
	if r.session == nil {
		return nil, domain.ErrUnavailable.WithMessage("amqp091: no session")
	}
	conn := r.session.Connection()
	if conn == nil {
		return nil, domain.ErrUnavailable.WithMessage("amqp091: session not connected")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, MapError(err)
	}
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace,
			"amqp091: receiver channel opened",
			"queue", r.cfg.QueueName,
		)
	}
	return ch, nil
}

func (r *Receiver) waitForReconnect(ctx context.Context) bool {
	if r.session == nil {
		return false
	}
	// Use Subscribe so multiple receivers (and any user-side observer
	// reading from Events()) all receive the SessionConnected event
	// independently. Reading from Events() directly would steal the
	// notification from siblings and cause them to hang forever.
	events, unsub := r.session.Subscribe()
	defer unsub()

	// Race window: between the receiver's consumeLoop returning (channel
	// lost) and reaching this point, the session may have ALREADY
	// reconnected and emitted SessionConnected. That event was delivered
	// to whatever subscribers existed at the time and is gone — we
	// subscribed too late. Probe the current health up front so we
	// proceed immediately when the session is already healthy, instead
	// of hanging until ctx expires.
	if h := r.session.Health(ctx); h.Connected {
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(context.Background(), logging.LevelTrace,
				"amqp091: receiver reconnect probe found session already connected",
				"queue", r.cfg.QueueName,
			)
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if ev.Type == ports.SessionConnected || ev.Type == ports.SessionReconciled {
				if logging.TraceEnabled(r.logger) {
					r.logger.Log(context.Background(), logging.LevelTrace,
						"amqp091: receiver reconnect signal received",
						"queue", r.cfg.QueueName,
						"event_type", ev.Type,
					)
				}
				return true
			}
		}
	}
}

func deliveryToEnvelope(d amqp.Delivery, clk clock.Clock) *domain.Envelope {
	if clk == nil {
		clk = clock.System
	}
	env := &domain.Envelope{
		ID:        d.MessageId,
		Subject:   d.RoutingKey,
		Payload:   d.Body,
		Headers:   deliveryToHeaders(d),
		CreatedAt: clk.Now(),
	}
	if env.ID == "" {
		env.ID = generateEnvelopeID()
	}
	if !d.Timestamp.IsZero() {
		env.CreatedAt = d.Timestamp
	}
	return env
}

func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func generateConsumerTag() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("amqp091: crypto/rand unavailable: " + err.Error())
	}
	return "gobridge-" + hex.EncodeToString(b)
}
