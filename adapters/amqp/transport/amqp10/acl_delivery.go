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

	mu sync.Mutex
	// settled: a settlement attempt has been claimed (may still be in
	// flight). settleDone: that attempt has finished. settleOK: it
	// finished successfully. The three flags let concurrent callers
	// distinguish "in progress" from "previously failed" (finding 17).
	settled    bool
	settleDone bool
	settleOK   bool

	// onSettled, when set (by the owning Receiver), is invoked exactly
	// once after the first settlement attempt completes — success or
	// failure — so the receiver can track in-flight deliveries and
	// bound its Close on graceful shutdown. Guarded by onSettledOnce.
	onSettled     func()
	onSettledOnce sync.Once

	// onSettleFailed, when set (by the owning Receiver), is invoked at
	// most once when the FIRST settlement attempt FAILS. A failed settle
	// permanently consumes this delivery's link-credit slot (go-amqp only
	// replenishes credit on a completed disposition), so the receiver
	// uses this to count failures and force a link rebuild before credit
	// exhaustion stalls it silently. Guarded by
	// onSettleFailedOnce.
	onSettleFailed     func(err error)
	onSettleFailedOnce sync.Once

	// delayWarnOnce dedupes the "delayed retry" Warn to once
	// per receiver link. It is shared by every Delivery created from the
	// same link (set by receiverLink.Receive) so an unhonored delayed
	// retry warns once per link rather than once per message (G). A nil
	// guard (directly-constructed deliveries) warns on each call.
	delayWarnOnce *sync.Once
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

// alreadySettledError reports the state of a delivery whose settlement
// was already claimed by an earlier call. A concurrent second caller
// used to get a misleading "already settled with error" while the first
// attempt was merely still IN FLIGHT; the in-progress case now has its
// own message (finding 17). Callers must hold no locks.
func (d *Delivery) alreadySettledError() error {
	d.mu.Lock()
	done, ok := d.settleDone, d.settleOK
	d.mu.Unlock()
	if ok {
		return nil
	}
	if !done {
		return shared.ErrUnavailable.WithMessage("amqp10: delivery settlement already in progress")
	}
	return shared.ErrUnavailable.WithMessage("amqp10: delivery settlement previously failed")
}

// fireOnSettled invokes the receiver's in-flight tracking hook exactly
// once. Safe to call multiple times and with a nil hook.
func (d *Delivery) fireOnSettled() {
	d.onSettledOnce.Do(func() {
		if d.onSettled != nil {
			d.onSettled()
		}
	})
}

// fireOnSettleFailed invokes the receiver's settlement-failure hook
// exactly once. Safe to call with a nil hook.
func (d *Delivery) fireOnSettleFailed(err error) {
	d.onSettleFailedOnce.Do(func() {
		if d.onSettleFailed != nil {
			d.onSettleFailed(err)
		}
	})
}

// finishSettle records the outcome of a settlement attempt, fires the
// in-flight tracking hook, and — on failure — notifies the owning
// Receiver so it can observe the leaked link credit and force a link
// rebuild before the receiver stalls silently.
func (d *Delivery) finishSettle(err error) {
	d.mu.Lock()
	d.settleDone = true
	d.settleOK = err == nil
	d.mu.Unlock()
	d.fireOnSettled()
	if err != nil {
		d.fireOnSettleFailed(err)
	}
}

// Ack settles the message with an accepted disposition. The settlement
// is idempotent — only the first call performs the operation. If a
// prior settlement attempt failed (or is still in flight), subsequent
// calls return ErrUnavailable to signal the unsettled state.
func (d *Delivery) Ack(ctx context.Context) error {
	d.mu.Lock()
	if d.settled {
		d.mu.Unlock()
		return d.alreadySettledError()
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

	d.finishSettle(err)
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Retry releases or modifies the message for redelivery.
//
// When after is zero, the message is released for immediate redelivery
// (ReleaseMessage). When after is positive, the message is modified with
// DeliveryFailed=true (ModifyMessage) to hand it back to the broker for
// redelivery, and the requested delay is attached as the
// x-opt-delivery-time message annotation (absolute ms-epoch scheduled
// delivery time) merged via the modified outcome. Brokers with
// scheduled-delivery support (e.g. Artemis) honor the annotation;
// brokers without it fall back to their redelivery-delay policy, so the
// delay remains best-effort — surfaced via
// MetricAMQP10DelayedRetryDeferred and a once-per-link Warn. The
// message is never dropped, which preserves at-least-once delivery (the
// safe choice over returning ErrNotSupported, which would route every
// backed-off retry straight to the DLQ).
//
// If a prior settlement attempt failed (or is in flight), subsequent
// calls return ErrUnavailable.
func (d *Delivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.mu.Lock()
	if d.settled {
		d.mu.Unlock()
		return d.alreadySettledError()
	}
	d.settled = true
	d.mu.Unlock()

	var err error
	if after > 0 {
		// Finding 2 (delayed-retry boundary): the broker, not this
		// client, ultimately controls redelivery timing for a modified
		// outcome. The x-opt-delivery-time annotation asks the broker to
		// schedule redelivery at now+after; a honoring broker applies the
		// spacing, a non-honoring one falls back to its own policy.
		// Surface the broker-delegated scheduling two ways (G): a
		// per-message counter for rate/alerting and a once-per-link Warn
		// (deduped via delayWarnOnce).
		d.metrics.Counter(MetricAMQP10DelayedRetryDeferred, 1)
		d.warnDelayedRetryDeferred(after)
		err = d.settle.ModifyMessage(ctx, d.msg, &amqp.ModifyMessageOptions{
			DeliveryFailed:    true,
			UndeliverableHere: false,
			Annotations: amqp.Annotations{
				annotationDeliveryTime: d.clk.Now().Add(after).UnixMilli(),
			},
		})
	} else {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "amqp10: releasing message",
				"envelope_id", d.env.ID(),
			)
		}
		err = d.settle.ReleaseMessage(ctx, d.msg)
	}

	d.finishSettle(err)
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Extend is not supported by AMQP 1.0. The protocol uses credit-based
// flow control rather than visibility timeouts.
func (d *Delivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}

// annotationDeliveryTime is the broker message annotation carrying an
// absolute scheduled-delivery time in milliseconds since epoch. Merged
// into the message via the modified outcome's annotations so brokers
// with scheduled-delivery support (e.g. ActiveMQ Artemis) space out the
// redelivery per the runtime's requested backoff.
const annotationDeliveryTime = "x-opt-delivery-time"

// warnDelayedRetryDeferred emits a single Warn per receiver link when a
// delayed retry's spacing is deferred to broker scheduling. delayWarnOnce
// is shared by every Delivery created from the same link, so the warning
// fires once per link rather than once per message;
// MetricAMQP10DelayedRetryDeferred still records every occurrence. A nil
// guard (directly-constructed deliveries) warns on each call.
func (d *Delivery) warnDelayedRetryDeferred(after time.Duration) {
	if d.logger == nil {
		return
	}
	emit := func() {
		d.logger.Warn(
			"amqp10: delayed retry deferred to broker scheduling; x-opt-delivery-time annotation attached, broker controls redelivery timing",
			"envelope_id", d.env.ID(),
			"requested_delay", after,
		)
	}
	if d.delayWarnOnce != nil {
		d.delayWarnOnce.Do(emit)
		return
	}
	emit()
}
