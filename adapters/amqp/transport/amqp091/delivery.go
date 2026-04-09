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

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an incoming AMQP 0-9-1 message.
// Settlement is guaranteed at-most-once via sync.Once.
type Delivery struct {
	env     *domain.Envelope
	raw     amqp.Delivery
	logger  *slog.Logger
	metrics ports.MetricsExporter
	once    sync.Once
}

// NewDelivery wraps an amqp091.Delivery as a ports.Delivery.
func NewDelivery(env *domain.Envelope, raw amqp.Delivery, logger *slog.Logger, metrics ports.MetricsExporter) *Delivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	return &Delivery{
		env:     env,
		raw:     raw,
		logger:  logger,
		metrics: metrics,
	}
}

func (d *Delivery) Envelope() *domain.Envelope { return d.env }

// Ack acknowledges the single message to the broker.
func (d *Delivery) Ack(ctx context.Context) error {
	var err error
	d.once.Do(func() {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp091: acking",
				"delivery_tag", d.raw.DeliveryTag,
			)
		}
		ackStart := time.Now()
		err = d.raw.Ack(false)
		d.metrics.Timer(domain.MetricAMQP091AckLatency, time.Since(ackStart),
			domain.Tag{Key: domain.TagKeyEntity, Value: d.raw.RoutingKey})
	})
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Retry nacks the message with requeue=true for immediate redelivery.
// The after parameter is logged but AMQP 0-9-1 does not support delayed
// redelivery natively; the message is requeued immediately.
func (d *Delivery) Retry(ctx context.Context, after time.Duration, reason error) error {
	var err error
	d.once.Do(func() {
		if after > 0 && d.logger != nil {
			d.logger.Warn("amqp091: delayed retry not natively supported, requeueing immediately",
				"delivery_tag", d.raw.DeliveryTag,
				"requested_delay", after,
				"reason", reason,
			)
		}
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp091: nacking (requeue)",
				"delivery_tag", d.raw.DeliveryTag,
			)
		}
		err = d.raw.Nack(false, true)
	})
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Extend is not supported by AMQP 0-9-1 (no visibility timeout concept).
func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return domain.ErrNotSupported
}
