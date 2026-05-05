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

// ExchangeDeclare declares an exchange.
func (c *amqpChannel) ExchangeDeclare(name, kind string, durable, autoDelete bool) error {
	if err := c.raw.ExchangeDeclare(name, kind, durable, autoDelete, false, false, nil); err != nil {
		return fmt.Errorf("amqp091: exchange declare: %w", err)
	}
	return nil
}

// QueueDeclare declares a queue.
func (c *amqpChannel) QueueDeclare(name string, durable, autoDelete bool) error {
	if _, err := c.raw.QueueDeclare(name, durable, autoDelete, false, false, nil); err != nil {
		return fmt.Errorf("amqp091: queue declare: %w", err)
	}
	return nil
}

// QueueBind binds a queue to an exchange with the given routing key.
func (c *amqpChannel) QueueBind(name, key, exchange string) error {
	if err := c.raw.QueueBind(name, key, exchange, false, nil); err != nil {
		return fmt.Errorf("amqp091: queue bind: %w", err)
	}
	return nil
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
func (c *amqpChannel) Consume(
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
	go func() {
		defer close(out)
		for d := range deliveries {
			env := deliveryToEnvelope(d, clk)
			out <- NewDelivery(env, d, logger, metrics, clk)
		}
	}()
	return out, nil
}

// publishContext publishes a single *amqp091.Publishing built from the
// envelope, on the underlying channel. Used by the senderChannel wrapper.
func (c *amqpChannel) publishContext(
	ctx context.Context,
	exchange, routingKey string,
	mandatory, immediate bool,
	pub amqp.Publishing,
) error {
	if err := c.raw.PublishWithContext(ctx, exchange, routingKey, mandatory, immediate, pub); err != nil {
		return fmt.Errorf("amqp091: publish: %w", err)
	}
	return nil
}
