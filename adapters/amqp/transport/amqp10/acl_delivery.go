package amqp10

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// settler is the subset of *amqp.Receiver used for message settlement.
// It enables test-double injection.
type settler interface {
	AcceptMessage(ctx context.Context, msg *amqp.Message) error
	ReleaseMessage(ctx context.Context, msg *amqp.Message) error
	ModifyMessage(ctx context.Context, msg *amqp.Message, options *amqp.ModifyMessageOptions) error
}

var _ ports.Delivery = (*Delivery)(nil)

// Delivery implements ports.Delivery for an AMQP 1.0 message.
// Settlement operations map to AMQP 1.0 disposition outcomes:
//   - Ack:    AcceptMessage
//   - Retry:  ReleaseMessage or ModifyMessage
//   - Extend: not supported (credit-based flow control)
type Delivery struct {
	env     *domain.Envelope
	msg     *amqp.Message
	settle  settler
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu       sync.Mutex
	settled  bool
	settleOK bool
}

// NewDelivery wraps an AMQP 1.0 message as a ports.Delivery.
func NewDelivery(
	env *domain.Envelope,
	msg *amqp.Message,
	settle settler,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) *Delivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	if clk == nil {
		clk = clock.System
	}
	return &Delivery{
		env:     env,
		msg:     msg,
		settle:  settle,
		logger:  logger,
		metrics: metrics,
		clk:     clk,
	}
}

func (d *Delivery) Envelope() *domain.Envelope { return d.env }

// Ack settles the message with an accepted disposition. The settlement
// is idempotent — only the first successful call performs the operation.
// If a prior settlement attempt failed, subsequent calls return
// ErrUnavailable to signal the unsettled state.
func (d *Delivery) Ack(ctx context.Context) error {
	d.mu.Lock()
	if d.settled {
		ok := d.settleOK
		d.mu.Unlock()
		if ok {
			return nil
		}
		return domain.ErrUnavailable.WithMessage("amqp10: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "amqp10: accepting message",
			"envelope_id", d.env.ID,
		)
	}

	start := d.clk.Now()
	err := d.settle.AcceptMessage(ctx, d.msg)
	d.metrics.Timer(domain.MetricAMQP10AcceptLatency, d.clk.Since(start))

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Retry releases or modifies the message for redelivery.
// When after is zero, the message is released for immediate redelivery.
// When after is positive, the message is modified with DeliveryFailed=true
// to signal the broker that a retry is needed.
// If a prior settlement attempt failed, subsequent calls return
// ErrUnavailable.
func (d *Delivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.mu.Lock()
	if d.settled {
		ok := d.settleOK
		d.mu.Unlock()
		if ok {
			return nil
		}
		return domain.ErrUnavailable.WithMessage("amqp10: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	var err error
	if after > 0 {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp10: modifying message for retry",
				"envelope_id", d.env.ID,
				"delay", after,
			)
		}
		err = d.settle.ModifyMessage(ctx, d.msg, &amqp.ModifyMessageOptions{
			DeliveryFailed:    true,
			UndeliverableHere: false,
		})
	} else {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp10: releasing message",
				"envelope_id", d.env.ID,
			)
		}
		err = d.settle.ReleaseMessage(ctx, d.msg)
	}

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Extend is not supported by AMQP 1.0. The protocol uses credit-based
// flow control rather than visibility timeouts.
func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return domain.ErrNotSupported
}
