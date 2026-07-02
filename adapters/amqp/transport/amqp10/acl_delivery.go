package amqp10

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	env     *messaging.Envelope
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
//
// The *amqp.Message parameter is the SDK boundary input this ACL
// constructor exists to wrap; it is injected by the ACL receiver and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewDelivery(
	env *messaging.Envelope,
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

func (d *Delivery) Envelope() *messaging.Envelope { return d.env }

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
		return shared.ErrUnavailable.WithMessage("amqp10: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "amqp10: accepting message",
			"envelope_id", d.env.ID(),
		)
	}

	start := d.clk.Now()
	err := d.settle.AcceptMessage(ctx, d.msg)
	d.metrics.Timer(MetricAMQP10AcceptLatency, d.clk.Since(start))

	if err != nil {
		return MapError(err)
	}

	d.mu.Lock()
	d.settleOK = true
	d.mu.Unlock()
	return nil
}

// Retry releases or modifies the message for redelivery.
//
// When after is zero, the message is released for immediate redelivery
// (ReleaseMessage). When after is positive, the message is modified with
// DeliveryFailed=true (ModifyMessage) to hand it back to the broker for
// redelivery. AMQP 1.0 has no portable client-side delayed-redelivery
// primitive, so the requested delay is ADVISORY: the broker — not this
// client — controls when the message is redelivered. The message is
// never dropped, which preserves at-least-once delivery (the safe choice
// over returning ErrNotSupported, which would route every backed-off
// retry straight to the DLQ).
//
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
		return shared.ErrUnavailable.WithMessage("amqp10: delivery already settled with error")
	}
	d.settled = true
	d.mu.Unlock()

	var err error
	if after > 0 {
		// Finding 2 (delayed-retry boundary): the broker, not this
		// client, controls redelivery timing for a modified outcome, so
		// the runtime's requested backoff is not honored here. Surface it
		// at Debug (rather than silently) so operators can see when a
		// configured backoff is effectively broker-driven.
		if logging.DebugEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelDebug,
				"amqp10: delayed retry not honored client-side; broker controls redelivery timing",
				"envelope_id", d.env.ID(),
				"requested_delay", after,
			)
		}
		err = d.settle.ModifyMessage(ctx, d.msg, &amqp.ModifyMessageOptions{
			DeliveryFailed:    true,
			UndeliverableHere: false,
		})
	} else {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp10: releasing message",
				"envelope_id", d.env.ID(),
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
	return shared.ErrNotSupported
}
