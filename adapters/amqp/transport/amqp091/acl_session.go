package amqp091

import (
	"context"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// amqpChannel is an unexported wrapper around *amqp091.Channel. It
// exposes only the operations the adapter needs, in domain-typed form,
// so that consumers (Session, Receiver) outside this ACL never have to
// reference the SDK directly.
type amqpChannel struct {
	raw *amqp.Channel
}

// Close closes the underlying AMQP channel.
func (c *amqpChannel) Close() error {
	if err := c.raw.Close(); err != nil {
		return fmt.Errorf("amqp091: close channel: %w", err)
	}
	return nil
}

// Qos applies the channel's prefetch settings (count and size).
func (c *amqpChannel) Qos(prefetchCount, prefetchSize int) error {
	if err := c.raw.Qos(prefetchCount, prefetchSize, false); err != nil {
		return fmt.Errorf("amqp091: qos: %w", err)
	}
	return nil
}

// ExchangeDeclare declares an exchange. args carries optional
// exchange-declaration arguments (e.g. alternate-exchange, x-delayed-type).
func (c *amqpChannel) ExchangeDeclare(name, kind string, durable, autoDelete bool, args map[string]any) error {
	if err := c.raw.ExchangeDeclare(name, kind, durable, autoDelete, false, false, toAMQPTable(args)); err != nil {
		return fmt.Errorf("amqp091: exchange declare: %w", err)
	}
	return nil
}

// QueueDeclare declares a queue. args carries optional queue-declaration
// arguments (e.g. x-queue-type=quorum, x-dead-letter-exchange, x-message-ttl).
func (c *amqpChannel) QueueDeclare(name string, durable, autoDelete bool, args map[string]any) error {
	if _, err := c.raw.QueueDeclare(name, durable, autoDelete, false, false, toAMQPTable(args)); err != nil {
		return fmt.Errorf("amqp091: queue declare: %w", err)
	}
	return nil
}

// QueueBind binds a queue to an exchange with the given routing key.
// args carries optional binding arguments (e.g. x-match plus headers for
// a headers exchange).
func (c *amqpChannel) QueueBind(name, key, exchange string, args map[string]any) error {
	if err := c.raw.QueueBind(name, key, exchange, false, toAMQPTable(args)); err != nil {
		return fmt.Errorf("amqp091: queue bind: %w", err)
	}
	return nil
}

// toAMQPTable converts a domain-typed argument map (sourced from the
// typed plugin config) into an amqp.Table for topology declarations.
// Nested maps and slices are converted recursively so structured
// arguments survive; scalar values pass through unchanged. A nil/empty
// map yields a nil Table, which the SDK treats as "no arguments".
//
// Unsupported Go value types are not rejected here — the SDK's table
// encoder validates them at declare time and the resulting error is
// surfaced through MapError, so misconfigured arguments fail fast with a
// clear message rather than being silently dropped.
func toAMQPTable(m map[string]any) amqp.Table {
	if len(m) == 0 {
		return nil
	}
	t := make(amqp.Table, len(m))
	for k, v := range m {
		t[k] = toAMQPValue(v)
	}
	return t
}

func toAMQPValue(v any) any {
	switch vv := v.(type) {
	case map[string]any:
		return toAMQPTable(vv)
	case []any:
		out := make([]any, len(vv))
		for i, e := range vv {
			out[i] = toAMQPValue(e)
		}
		return out
	default:
		return vv
	}
}

// NotifyClose returns a single-use channel that emits at most one
// error (the channel-close cause) and is then closed. Graceful closes
// deliver no value before closing.
func (c *amqpChannel) NotifyClose() <-chan error {
	in := c.raw.NotifyClose(make(chan *amqp.Error, 1))
	out := make(chan error, 1)
	go func() {
		defer close(out)
		e, ok := <-in
		if !ok {
			return
		}
		if e != nil {
			out <- e
		}
	}()
	return out
}

// Consume starts a consumer on this channel and returns a stream of
// already-wrapped *Delivery values. The returned channel is closed
// when the underlying SDK delivery stream is closed.
//
// ctx bounds the forwarding goroutine's lifetime: when the caller
// cancels (receiver shutdown / reconnect), the goroutine stops blocking
// on the unbuffered out channel and returns instead of leaking. Any
// delivery already converted but not yet handed over is nacked with
// requeue=true so it is redelivered (at-least-once) rather than dropped.
func (c *amqpChannel) Consume(
	ctx context.Context,
	queue, consumerTag string,
	autoAck, exclusive bool,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) (<-chan *Delivery, error) {
	deliveries, err := c.raw.Consume(
		queue,
		consumerTag,
		autoAck,
		exclusive,
		false, // noLocal: RabbitMQ does not support this
		false, // noWait
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("amqp091: consume: %w", err)
	}

	out := make(chan *Delivery)
	go forwardDeliveries(ctx, deliveries, out, autoAck, logger, metrics, clk)
	return out, nil
}

// forwardDeliveries converts raw SDK deliveries into domain *Delivery
// values and forwards them on out until deliveries is closed or ctx is
// cancelled. Malformed deliveries are rejected (no requeue) and skipped.
//
// ctx bounds the goroutine: a send on the unbuffered out channel is
// raced against ctx.Done() so that when the consumer is shutting down or
// reconnecting (and nothing is reading out), the goroutine returns
// instead of leaking. A delivery already converted but not yet handed
// over is nacked with requeue=true (in manual-ack mode) so it is
// redelivered rather than dropped — the safest at-least-once choice.
func forwardDeliveries(
	ctx context.Context,
	deliveries <-chan amqp.Delivery,
	out chan<- *Delivery,
	autoAck bool,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) {
	defer close(out)
	for d := range deliveries {
		env, err := deliveryToEnvelope(d, clk)
		if err != nil {
			if logger != nil {
				logger.Warn("amqp091: dropping malformed delivery",
					"error", err,
					"message_id", d.MessageId,
				)
			}
			_ = d.Reject(false)
			continue
		}
		select {
		case out <- NewDelivery(env, d, logger, metrics, clk):
		case <-ctx.Done():
			// Shutdown/reconnect: do not leak this goroutine blocking
			// on a reader that is gone. Requeue the in-flight delivery
			// (broker also requeues unacked on channel close, but this
			// is prompt and explicit). Nack may fail if the channel is
			// already closing; the broker requeue covers that case.
			if !autoAck {
				_ = d.Nack(false, true)
			}
			return
		}
	}
}

// publishContext publishes a single *amqp091.Publishing built from the
// envelope, on the underlying channel. Used by the senderChannel wrapper.
//
// The AMQP basic.publish "immediate" flag is intentionally hard-wired to
// false: RabbitMQ removed support for it in 3.0 and closes the channel
// when it is set, so there is no safe value other than false. The managed
// config layer (Config.Validate) rejects sender.immediate=true outright.
func (c *amqpChannel) publishContext(
	ctx context.Context,
	exchange, routingKey string,
	mandatory bool,
	pub amqp.Publishing,
) error {
	const immediate = false
	if err := c.raw.PublishWithContext(ctx, exchange, routingKey, mandatory, immediate, pub); err != nil {
		return fmt.Errorf("amqp091: publish: %w", err)
	}
	return nil
}
