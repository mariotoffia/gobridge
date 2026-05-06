package amqp091

import (
	"context"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an incoming AMQP 0-9-1 message.
// Settlement is guaranteed at-most-once via a mutex-guarded flag that
// tracks whether the delivery has been settled (and whether it succeeded).
type Delivery struct {
	env     *messaging.Envelope
	raw     amqp.Delivery
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu       sync.Mutex
	settled  bool
	settleOK bool
}

// NewDelivery wraps an amqp091.Delivery as a ports.Delivery.
func NewDelivery(env *messaging.Envelope, raw amqp.Delivery, logger *slog.Logger, metrics ports.MetricsExporter, clk clock.Clock) *Delivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	if clk == nil {
		clk = clock.System
	}
	return &Delivery{
		env:     env,
		raw:     raw,
		logger:  logger,
		metrics: metrics,
		clk:     clk,
	}
}

func (d *Delivery) Envelope() *messaging.Envelope { return d.env }

// Ack acknowledges the single message to the broker. The settlement
// is idempotent for successful calls. If a prior settlement attempt
// failed, subsequent calls return ErrUnavailable.
func (d *Delivery) Ack(ctx context.Context) error {
	d.mu.Lock()
	if d.settled {
		ok := d.settleOK
		d.mu.Unlock()
		if ok {
			return nil
		}
		return shared.ErrUnavailable.WithMessage("amqp091: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "amqp091: acking",
			"delivery_tag", d.raw.DeliveryTag,
		)
	}
	ackStart := d.clk.Now()
	err := d.raw.Ack(false)
	d.metrics.Timer(shared.MetricAMQP091AckLatency, d.clk.Since(ackStart),
		shared.Tag{Key: shared.TagKeyEntity, Value: d.raw.RoutingKey})

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Retry nacks the message with requeue=true for immediate redelivery.
// The after parameter is logged but AMQP 0-9-1 does not support delayed
// redelivery natively; the message is requeued immediately.
// If a prior settlement attempt failed, subsequent calls return
// ErrUnavailable.
func (d *Delivery) Retry(ctx context.Context, after time.Duration, reason error) error {
	d.mu.Lock()
	if d.settled {
		ok := d.settleOK
		d.mu.Unlock()
		if ok {
			return nil
		}
		return shared.ErrUnavailable.WithMessage("amqp091: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	if after > 0 && d.logger != nil {
		logging.Debug(d.logger, "amqp091: delayed retry not natively supported, requeueing immediately",
			"delivery_tag", d.raw.DeliveryTag,
			"requested_delay", after,
		)
	}
	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "amqp091: nacking (requeue)",
			"delivery_tag", d.raw.DeliveryTag,
		)
	}
	err := d.raw.Nack(false, true)

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Extend is not supported by AMQP 0-9-1 (no visibility timeout concept).
func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
