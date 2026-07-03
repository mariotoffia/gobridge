package sqs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Delivery = (*sqsDelivery)(nil)

const (
	// sqsMaxVisibilitySeconds is the SQS hard cap for a message
	// VisibilityTimeout (12 hours).
	sqsMaxVisibilitySeconds int32 = 43200

	// minAutoExtendVisibilitySeconds is the smallest visibility timeout
	// the auto-extend machinery will use. Below 2s the derived tick
	// interval (visibility/2) rounds down toward a non-positive duration,
	// and a clock Ticker panics when NewTicker/Reset is handed d <= 0. It
	// is also the threshold newDelivery uses to decide whether starting
	// the auto-extend goroutine is worthwhile at all.
	minAutoExtendVisibilitySeconds int32 = 2

	// sqsSettlementTimeout bounds the final Ack/Retry SQS call when the
	// caller's context is already cancelled at settlement time (typically
	// graceful shutdown racing a completed send). See settlementContext.
	sqsSettlementTimeout = 10 * time.Second
)

// settlementContext returns the context for the final Ack/Retry SQS
// call. A live ctx is used as-is. When ctx is already cancelled —
// shutdown cancelled the delivery context after the send completed —
// cancellation is stripped with context.WithoutCancel (values such as
// trace/correlation are kept) and the call is bounded by
// sqsSettlementTimeout instead, mirroring the runtime's panic-path
// settlement pattern (runtime/route/runner.go). Without this, a
// delivery whose egress finished at shutdown would fail its
// DeleteMessage on the cancelled ctx, guaranteeing duplicate egress on
// restart for every in-flight message.
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), sqsSettlementTimeout)
}

// clampVisibilitySeconds bounds a desired visibility timeout (seconds)
// into the range the auto-extend machinery can apply safely. It takes
// int64 so callers convert Duration→seconds BEFORE any int32 narrowing:
// a naive int32(hugeDuration.Seconds()) is an unspecified conversion in
// Go and can wrap negative, turning a "hold this message for a long
// time" request into a near-immediate redelivery.
//
//   - Lower bound minAutoExtendVisibilitySeconds (2s): an Extend whose
//     deadline resolves to "now or in the past" (delta <= 0) MUST NOT be
//     turned into a 0-second ChangeMessageVisibility — that would make
//     the still-in-flight message immediately visible to another
//     consumer and cause duplicate processing. The same floor guarantees
//     the auto-extend loop derives a strictly positive tick interval, so
//     Ticker.Reset / NewTicker never panic (both reject d <= 0).
//   - Upper bound sqsMaxVisibilitySeconds (43200s): the SQS hard cap.
func clampVisibilitySeconds(seconds int64) int32 {
	if seconds < int64(minAutoExtendVisibilitySeconds) {
		return minAutoExtendVisibilitySeconds
	}
	if seconds > int64(sqsMaxVisibilitySeconds) {
		return sqsMaxVisibilitySeconds
	}
	return int32(seconds)
}

// autoExtendInterval returns the tick interval for the auto-extend loop
// given the current visibility timeout (seconds): half the visibility
// window, floored at 1s so the clock Ticker never receives a
// non-positive duration (NewTicker / Reset panic on d <= 0). It is the
// defensive counterpart to clampVisibilitySeconds — even if a degenerate
// visibility ever reached the loop, the ticker stays valid.
func autoExtendInterval(visSeconds int32) time.Duration {
	d := time.Duration(visSeconds) * time.Second / 2
	if d < time.Second {
		return time.Second
	}
	return d
}

// sqsDelivery wraps a received SQS message and maps the ports.Delivery
// lifecycle (Ack, Retry, Extend) to SQS receipt-handle operations.
//
// # Context hierarchy
//
// The receiver's poll loop creates a three-level context tree per message:
//
//		pollLoop ctx (caller-owned, e.g. test WithTimeout)
//		  └─ deliveryCtx (WithCancel) — passed to emit() and to newDelivery
//		       └─ autoExtendCtx (WithCancel) — scoped to the auto-extend goroutine
//
//	  - deliveryCtx is canceled by cleanupContext() after the Ack/Retry SQS
//	    call completes, to reclaim the context node.
//	  - autoExtendCtx is canceled by stopAutoExtend() before the SQS call,
//	    so the goroutine stops before we delete/re-queue the message.
//	  - If auto-extend fails 3 consecutive times, it calls processingCancel
//	    (= deliveryCtx cancel) so the emit callback receives a canceled ctx.
//
// # Why separate stopAutoExtend and cleanupContext
//
// Ack and Retry must stop the auto-extend goroutine before calling the
// SQS API (otherwise the goroutine might race a ChangeMessageVisibility
// against the DeleteMessage). But they must NOT cancel deliveryCtx before
// the API call, because the caller passes deliveryCtx as the ctx argument
// to Ack/Retry. Canceling it early would make the SQS call fail with
// "context canceled". Therefore:
//
//   - stopAutoExtend runs first  → stops the goroutine only
//   - SQS API call runs          → uses the still-live deliveryCtx
//   - cleanupContext runs (defer) → frees the deliveryCtx node
type sqsDelivery struct {
	env               *messaging.Envelope
	client            sqsAPI
	queueURL          string
	receiptHandle     string
	visibilityTimeout atomic.Int32
	logger            *slog.Logger
	metrics           ports.MetricsExporter
	clk               clock.Clock

	cancel           context.CancelFunc // cancels autoExtendCtx (stops the goroutine)
	processingCancel context.CancelFunc // cancels deliveryCtx (frees the context node)
	once             sync.Once          // ensures stopAutoExtend is idempotent
}

// newDelivery creates a delivery for a received SQS message.
//
// When autoExtend is true and visibilityTimeout > 1s, a background
// goroutine (autoExtendLoop) periodically calls ChangeMessageVisibility
// at 50% of visibilityTimeout. This keeps the message invisible to
// other consumers while the bridge processes it. The loop runs until
// the delivery is finalized (Ack or Retry) or the parent context is
// canceled.
//
// processingCancel is the cancel func for deliveryCtx (see context
// hierarchy on sqsDelivery). It may be nil in tests.
//
// clk is the clock used to drive the auto-extend ticker. When nil it
// defaults to clock.System (wall clock). Tests pass a clocktest.Fake to
// control tick firing deterministically.
func newDelivery(
	parentCtx context.Context,
	env *messaging.Envelope,
	client sqsAPI,
	queueURL string,
	receiptHandle string,
	visibilityTimeout int32,
	autoExtend bool,
	processingCancel context.CancelFunc,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) *sqsDelivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	if clk == nil {
		clk = clock.System
	}

	ctx, cancel := context.WithCancel(parentCtx)

	d := &sqsDelivery{
		env:              env,
		client:           client,
		queueURL:         queueURL,
		receiptHandle:    receiptHandle,
		processingCancel: processingCancel,
		logger:           logger,
		metrics:          metrics,
		clk:              clk,
		cancel:           cancel,
	}
	d.visibilityTimeout.Store(visibilityTimeout)

	if autoExtend && visibilityTimeout >= minAutoExtendVisibilitySeconds {
		go d.autoExtendLoop(ctx)
	}

	return d
}

func (d *sqsDelivery) Envelope() *messaging.Envelope { return d.env }

// Ack deletes the SQS message, confirming successful processing.
//
// When ctx is already cancelled (shutdown racing a completed send) the
// DeleteMessage still runs under a bounded settlement context — see
// settlementContext — so a successfully egressed message is not
// redelivered on restart.
func (d *sqsDelivery) Ack(ctx context.Context) error {
	d.stopAutoExtend()
	defer d.cleanupContext()

	settleCtx, settleCancel := settlementContext(ctx)
	defer settleCancel()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: acking",
			"queue_url", d.queueURL,
			"message_id", d.env.ID,
		)
	}

	start := d.clk.Now()
	_, err := d.client.DeleteMessage(settleCtx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(d.queueURL),
		ReceiptHandle: aws.String(d.receiptHandle),
	})
	if err != nil {
		return MapError(err)
	}
	d.metrics.Timer(MetricSQSDeleteLatency, d.clk.Since(start),
		shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
	return nil
}

// Retry makes the message visible again after the given delay.
// A zero delay makes the message immediately available for re-delivery.
//
// The delay is clamped in int64 seconds BEFORE the int32 narrowing —
// int32(hugeDuration.Seconds()) is an unspecified conversion that can
// wrap negative, turning a long hold into near-immediate redelivery.
// Like Ack, Retry settles under settlementContext so a shutdown-
// cancelled ctx cannot prevent the nack from reaching SQS.
func (d *sqsDelivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.stopAutoExtend()
	defer d.cleanupContext()

	settleCtx, settleCancel := settlementContext(ctx)
	defer settleCancel()

	secs := int64(after / time.Second)
	switch {
	case secs < 0:
		secs = 0
	case secs == 0 && after > 0:
		secs = 1
	case secs > int64(sqsMaxVisibilitySeconds):
		secs = int64(sqsMaxVisibilitySeconds)
	}
	timeout := int32(secs)

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: retrying",
			"queue_url", d.queueURL,
			"message_id", d.env.ID,
			"delay_seconds", timeout,
		)
	}

	_, err := d.client.ChangeMessageVisibility(settleCtx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(d.queueURL),
		ReceiptHandle:     aws.String(d.receiptHandle),
		VisibilityTimeout: timeout,
	})
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Extend pushes the visibility timeout to the given absolute time.
// The delta from now is converted to whole seconds in int64 and clamped
// via clampVisibilitySeconds into
// [minAutoExtendVisibilitySeconds, sqsMaxVisibilitySeconds] BEFORE any
// int32 narrowing (a naive int32 conversion of a huge delta is
// unspecified and can wrap negative — near-immediate redelivery instead
// of a long hold). The lower floor is deliberate: an Extend whose
// deadline has already passed must NOT surface the in-flight message (a
// 0-second ChangeMessageVisibility would do exactly that and invite
// duplicate processing), and the floor also keeps the auto-extend tick
// interval strictly positive so the ticker can never be Reset with a
// panicking d <= 0.
//
// When auto-extend is running, Extend also updates the stored timeout
// atomically so the next auto-extend tick uses the new value. Without
// this, auto-extend would reset visibility to the old (shorter) value
// on the next tick, effectively undoing the Extend.
//
// Extend does NOT stop auto-extend — the goroutine keeps running and
// will maintain the new timeout. Call Ack or Retry to finalize.
func (d *sqsDelivery) Extend(ctx context.Context, until time.Time) error {
	timeout := clampVisibilitySeconds(int64(until.Sub(d.clk.Now()) / time.Second))

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: extending",
			"queue_url", d.queueURL,
			"message_id", d.env.ID,
			"new_timeout", timeout,
		)
	}

	_, err := d.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(d.queueURL),
		ReceiptHandle:     aws.String(d.receiptHandle),
		VisibilityTimeout: timeout,
	})
	if err != nil {
		return MapError(err)
	}

	d.metrics.Counter(MetricSQSVisibilityExtensions, 1,
		shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
	d.visibilityTimeout.Store(timeout)
	return nil
}

// stopAutoExtend cancels autoExtendCtx, which terminates the background
// goroutine. Idempotent — safe to call multiple times.
//
// Called before the SQS API call in Ack/Retry so that the goroutine
// cannot race a ChangeMessageVisibility against a DeleteMessage.
func (d *sqsDelivery) stopAutoExtend() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
	})
}

// cleanupContext cancels deliveryCtx, freeing the context tree node
// that the receiver's pollLoop allocated for this message.
//
// Called via defer after the SQS API call in Ack/Retry. Must NOT be
// called before the API call — the caller passes deliveryCtx as the
// ctx argument, so an early cancel would break the SQS request.
func (d *sqsDelivery) cleanupContext() {
	if d.processingCancel != nil {
		d.processingCancel()
	}
}

const autoExtendMaxFailures = 3

// autoExtendLoop is the background goroutine that keeps the message
// invisible to other consumers while processing is in progress.
//
// # How it works
//
// SQS makes a message invisible for VisibilityTimeout seconds after it
// is received. If the consumer doesn't delete (Ack) the message within
// that window, SQS redelivers it. For slow processors this creates a
// problem: the message becomes visible while the bridge is still working
// on it, causing a duplicate delivery.
//
// autoExtendLoop prevents this by calling ChangeMessageVisibility at
// 50% of the visibility timeout (the "tick interval"). Each call resets
// the invisibility clock to the full timeout value, giving the processor
// another full window to finish.
//
//	visibility = 30s → tick at 15s → extends to 30s from now → repeat
//
// # Dynamic visibility timeout
//
// If the caller calls Extend() (the ports.Delivery method), the stored
// visibilityTimeout is updated atomically. On the next tick the loop
// reads the new value and adjusts both the SQS call and its own tick
// interval. This is safe because ChangeMessageVisibility is idempotent
// per receipt handle — the last writer wins. Typical use case: a
// processor discovers it needs more time and calls Extend(now + 5m),
// which bumps visibilityTimeout to 300s and the tick to 150s.
//
// # Failure handling
//
// Transient SQS errors (network blip, throttle) are tolerated up to
// autoExtendMaxFailures consecutive times. The counter resets on each
// success, so interleaved failures do not accumulate. If the limit is
// reached, the loop calls processingCancel (cancels deliveryCtx),
// which signals the emit callback that processing should abort — the
// message will become visible again and SQS will redeliver it.
//
// # Lifetime
//
// The loop exits when ctx (autoExtendCtx) is canceled, which happens
// via stopAutoExtend() in Ack/Retry, or via parent context cancellation
// during graceful shutdown.
func (d *sqsDelivery) autoExtendLoop(ctx context.Context) {
	interval := autoExtendInterval(d.visibilityTimeout.Load())

	ticker := d.clk.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			vis := d.visibilityTimeout.Load()
			_, err := d.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(d.queueURL),
				ReceiptHandle:     aws.String(d.receiptHandle),
				VisibilityTimeout: vis,
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				consecutiveFailures++
				if d.logger != nil {
					d.logger.Warn("sqs: auto-extend visibility failed",
						"queue", d.queueURL,
						"error", err,
						"consecutive_failures", consecutiveFailures,
					)
				}
				if consecutiveFailures >= autoExtendMaxFailures {
					if d.logger != nil {
						d.logger.Error("sqs: auto-extend max failures reached, cancelling processing",
							"queue", d.queueURL,
							"message_id", d.env.ID,
							"consecutive_failures", consecutiveFailures,
						)
					}
					if d.processingCancel != nil {
						d.processingCancel()
					}
					return
				}
				continue
			}
			consecutiveFailures = 0
			newInterval := autoExtendInterval(vis)
			if newInterval != interval {
				ticker.Reset(newInterval)
				interval = newInterval
			}
			d.metrics.Counter(MetricSQSAutoExtends, 1,
				shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
			if logging.TraceEnabled(d.logger) {
				d.logger.Log(ctx, logging.LevelTrace, "sqs: auto-extended",
					"queue_url", d.queueURL,
					"message_id", d.env.ID,
					"visibility_timeout", vis,
				)
			}
		}
	}
}
