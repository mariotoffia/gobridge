package amqp091

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Receiver = (*Receiver)(nil)

// Receiver implements ports.Receiver for AMQP 0-9-1. It consumes
// messages from a queue via the Session's connection and emits
// each message as a ports.Delivery.
type Receiver struct {
	cfg     ReceiverConfig
	session *Session
	logger  *slog.Logger
	metrics ports.MetricsExporter
}

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	l := cfg.Logger
	if l == nil && cfg.Session != nil {
		l = cfg.Session.logger
	}
	return &Receiver{cfg: cfg, session: cfg.Session, logger: l, metrics: m}
}

// Run starts consuming messages from the configured queue. It blocks
// until ctx is cancelled or an unrecoverable error occurs. On channel
// or connection errors, it waits for the session to reconnect and
// re-establishes the consumer.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	logging.DebugContext(r.logger, ctx, "amqp091: receiver starting",
		"queue", r.cfg.QueueName,
		"consumer_tag", r.cfg.ConsumerTag,
	)

	for {
		err := r.consumeLoop(ctx, emit)
		if err == nil || ctx.Err() != nil {
			logging.DebugContext(r.logger, ctx, "amqp091: receiver stopped",
				"queue", r.cfg.QueueName, "reason", "context_cancelled")
			return ctx.Err()
		}

		if r.isEmitError(err) {
			return err
		}

		logging.DebugContext(r.logger, ctx, "amqp091: consumer channel lost, waiting for reconnect",
			"queue", r.cfg.QueueName, "error", err)

		if !r.waitForReconnect(ctx) {
			return ctx.Err()
		}
	}
}

// isEmitError returns true when the error originated from the emit callback
// rather than from the AMQP transport layer.
func (r *Receiver) isEmitError(err error) bool {
	var be *domain.BridgeError
	if errors.As(err, &be) {
		return false
	}
	return true
}

func (r *Receiver) consumeLoop(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	ch, err := r.openChannel()
	if err != nil {
		return MapError(err)
	}
	defer ch.Close()

	if r.cfg.PrefetchCount > 0 || r.cfg.PrefetchSize > 0 {
		if err := ch.Qos(r.cfg.PrefetchCount, r.cfg.PrefetchSize, false); err != nil {
			return MapError(err)
		}
	}

	consumerTag := r.cfg.ConsumerTag
	if consumerTag == "" {
		consumerTag = generateConsumerTag()
	}

	consumeStart := time.Now()
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
	r.metrics.Timer(domain.MetricAMQP091ConsumeLatency, time.Since(consumeStart),
		domain.Tag{Key: domain.TagKeyEntity, Value: r.cfg.QueueName})

	chanClose := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case <-ctx.Done():
			return nil
		case amqpErr := <-chanClose:
			if amqpErr != nil {
				return MapError(amqpErr)
			}
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return nil
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

	env := deliveryToEnvelope(d)
	del := NewDelivery(env, d, r.logger, r.metrics)

	if err := emit(ctx, del); err != nil {
		logging.DebugContext(r.logger, ctx, "amqp091: emit error",
			"queue", r.cfg.QueueName, "error", err)
		return err
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
	return conn.Channel()
}

func (r *Receiver) waitForReconnect(ctx context.Context) bool {
	if r.session == nil {
		return false
	}
	events := r.session.Events()
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if ev.Type == ports.SessionConnected || ev.Type == ports.SessionReconciled {
				return true
			}
		}
	}
}

func deliveryToEnvelope(d amqp.Delivery) *domain.Envelope {
	env := &domain.Envelope{
		ID:        d.MessageId,
		Subject:   d.RoutingKey,
		Payload:   d.Body,
		Headers:   deliveryToHeaders(d),
		CreatedAt: time.Now(),
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
