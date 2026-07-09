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

	// queue is the bounded queue name this delivery was consumed from. It
	// is the TagKeyEntity dimension for the Ack/Retry metrics: RoutingKey is
	// deliberately NOT used because it is caller-controlled and unbounded
	// (e.g. per-entity keys like "order.<id>"), which would explode metric
	// time-series cardinality. Set by forwardDeliveries; empty on
	// directly-constructed deliveries (mirrors receiver.go's ConsumeLatency
	// tag, which already uses the queue name).
	queue string

	mu       sync.Mutex
	settled  bool
	settleOK bool

	// delayWarnOnce dedupes the "delayed retry not honored" Warn to once
	// per consumer channel (shared by every Delivery the channel's
	// forwarder creates), so a poison message cannot flood the log while
	// MetricAMQP091DelayedRetryUnhonored still counts every occurrence.
	// Mirrors the amqp10 adapter's delayWarnOnce. Nil (directly
	// constructed deliveries) warns on each call.
	delayWarnOnce *sync.Once
}

// NewDelivery wraps an amqp091.Delivery as a ports.Delivery.
//
// The amqp.Delivery parameter is the SDK boundary input this ACL
// constructor exists to wrap; it is injected by the ACL receiver and
// stored behind unexported fields.
//
//aclcheck:allow-export
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
	d.metrics.Timer(MetricAMQP091AckLatency, d.clk.Since(ackStart),
		shared.Tag{Key: shared.TagKeyEntity, Value: d.queue})

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Retry nacks the message with requeue=true so the broker redelivers it.
// AMQP 0-9-1 has no native delayed-redelivery primitive, so when after>0 the
// runtime's requested backoff spacing is honored CLIENT-SIDE: the (still
// unacked) delivery is held for `after` — via the injected clock, cancellable
// by ctx — before the requeue. This spaces poison redeliveries instead of
// letting an immediate requeue hot-loop the message at the head of a classic
// queue (broker/CPU saturation, the poison-hot-loop finding). The condition is
// still surfaced two ways (parity with the amqp10 adapter): a per-message
// MetricAMQP091DelayedRetryUnhonored counter (the broker cannot honor the delay
// natively) for rate/alerting and a once-per-consumer-channel Warn. A genuinely
// poison message still returns forever — only spaced now, not hot — so guard it
// broker-side with x-delivery-limit (quorum queues) or a dead-letter-exchange on
// the queue declaration.
//
// The hold is cancellable: on ctx cancel (shutdown) the requeue happens at once
// so settlement never blocks teardown (the broker also requeues an unacked
// delivery on channel close). If a prior settlement attempt failed, subsequent
// calls return ErrUnavailable.
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

	if after > 0 {
		d.metrics.Counter(MetricAMQP091DelayedRetryUnhonored, 1,
			shared.Tag{Key: shared.TagKeyEntity, Value: d.queue})
		d.warnDelayedRetryUnhonored(after, reason)
		// Honor the requested backoff CLIENT-SIDE (AMQP 0-9-1 has no native
		// delayed redelivery): hold the unacked delivery for `after` before
		// requeueing, so a poison message is spaced instead of hot-looping.
		// A one-shot NewTimer (not After) so the timer is STOPPED — and its
		// resources released — on the ctx-cancel/shutdown path, instead of
		// lingering until `after` elapses; under a shutdown storm of many
		// long-`after` poison messages that would otherwise retain a timer
		// per delivery. Cancellable via the injected clock — no real timer —
		// so a shutdown/ctx-cancel requeues immediately.
		t := d.clk.NewTimer(after)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C():
		}
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

// warnDelayedRetryUnhonored emits a single Warn per consumer channel when a
// delayed retry is honored client-side (the delivery is held before requeue
// because AMQP 0-9-1 has no native delayed redelivery). delayWarnOnce is
// shared by every Delivery created from the same consume forwarder, so the
// warning fires once per channel rather than once per message;
// MetricAMQP091DelayedRetryUnhonored still records every occurrence. A nil
// guard (directly-constructed deliveries) warns on each call.
func (d *Delivery) warnDelayedRetryUnhonored(after time.Duration, reason error) {
	if d.logger == nil {
		return
	}
	emit := func() {
		d.logger.Warn(
			"amqp091: delayed retry honored client-side by holding the delivery before requeue "+
				"(AMQP 0-9-1 has no native delayed redelivery); a poison message is spaced but still "+
				"loops — guard it with x-delivery-limit or a dead-letter-exchange",
			"delivery_tag", d.raw.DeliveryTag,
			"routing_key", d.raw.RoutingKey,
			"requested_delay", after,
			"reason", reason,
		)
	}
	if d.delayWarnOnce != nil {
		d.delayWarnOnce.Do(emit)
		return
	}
	emit()
}
