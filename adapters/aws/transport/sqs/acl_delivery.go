package sqs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

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
	// the auto-extend machinery will use: clampVisibilitySeconds raises any
	// smaller window up to it, and newDelivery only starts the auto-extend
	// goroutine when the window is at least this. The tick interval itself
	// (visibility/3) is separately floored at 1s in autoExtendInterval, so
	// the clock Ticker never receives a non-positive duration.
	minAutoExtendVisibilitySeconds int32 = 2

	// sqsSettlementTimeout bounds every settlement/extend SQS call
	// (Ack/Retry/Extend/auto-extend). The SDK HTTP client has no overall
	// request timeout, so this is the only guard against a black-holed
	// connection wedging the delivery goroutine for the TCP RTO. See
	// settlementContext (Finding 5).
	sqsSettlementTimeout = 10 * time.Second

	// ackDeleteMarginSeconds is the minimum visibility (in seconds) Ack
	// guarantees remains on a message before it issues DeleteMessage
	// (Finding: c8-autoextend-margin). DeleteMessage is bounded by
	// sqsSettlementTimeout (10s); if the live visibility window were shorter,
	// the message could resurface to another consumer BEFORE the delete lands
	// — a duplicate (and, on a FIFO queue, group churn). A visibility_timeout
	// as low as 2-3s is otherwise permitted, so Ack performs a final
	// ChangeMessageVisibility to this floor whenever the remaining window is
	// below it, guaranteeing the delete always outruns redelivery. The floor
	// is the settlement bound plus a 5s buffer for scheduling/clock jitter.
	ackDeleteMarginSeconds int32 = int32(sqsSettlementTimeout/time.Second) + 5

	// marginCMVTimeout bounds the pre-delete visibility-margin
	// ChangeMessageVisibility (Finding: c8-autoextend-margin). It is
	// deliberately SHORT and INDEPENDENT of the delete's sqsSettlementTimeout
	// budget: the margin CMV and the DeleteMessage must not share one 10s
	// context, or a slow-but-eventually-successful CMV could starve the
	// delete's remaining budget and turn a would-have-succeeded delete into a
	// 15s-delayed redelivery (a duplicate). The CMV is best effort — on
	// timeout Ack still proceeds to the delete under the full settlement
	// budget — so a tight bound is safe.
	marginCMVTimeout = 3 * time.Second
)

// settlementContext returns the context for a settlement/extend SQS call
// (Ack/Retry/Extend/auto-extend). It ALWAYS bounds the call with
// sqsSettlementTimeout, even while ctx is still live (Finding 5): the SDK
// HTTP client has no overall request timeout, so a black-holed connection
// during DeleteMessage/ChangeMessageVisibility would otherwise wedge the
// delivery goroutine for the TCP RTO (tens of minutes) — holding a
// MaxInFlight slot and, for auto-extend, never incrementing the failure
// counter so processingCancel never fires.
//
// When ctx is already cancelled — shutdown cancelled the delivery context
// after the send completed — cancellation is stripped with
// context.WithoutCancel (values such as trace/correlation are kept) so the
// bounded call still reaches SQS and a successfully egressed message is
// not redelivered on restart, mirroring the runtime's panic-path
// settlement pattern (runtime/route/runner.go).
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundedSettleContext(ctx, sqsSettlementTimeout)
}

// boundedSettleContext bounds an SQS settlement/extend call with the given
// timeout, keeping settlementContext's cancellation semantics: while ctx is
// live it derives directly; once ctx is cancelled (shutdown) cancellation is
// stripped with context.WithoutCancel (values kept) so the bounded call still
// reaches SQS. It lets the pre-delete margin CMV get its OWN short bound
// (marginCMVTimeout) rather than sharing the delete's settlement budget.
func boundedSettleContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
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
// given the current visibility timeout (seconds): a THIRD of the
// visibility window (Finding 5), floored at 1s so the clock Ticker never
// receives a non-positive duration (NewTicker / Reset panic on d <= 0).
//
// Ticking at vis/3 (rather than vis/2) leaves margin after a single
// transient ChangeMessageVisibility failure: the first tick fires at
// vis/3, so a retry at the next tick (2·vis/3) still lands strictly
// before the window lapses at vis. At vis/2 the retry would fall at
// exactly the expiry instant, letting the message resurface to another
// consumer. It is also the defensive counterpart to
// clampVisibilitySeconds — even a degenerate visibility keeps the ticker
// valid.
func autoExtendInterval(visSeconds int32) time.Duration {
	d := time.Duration(visSeconds) * time.Second / 3
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
	// windowDeadline is the wall-clock instant (unix nanos) at which the
	// CURRENT visibility window lapses. It is seeded in newDelivery and
	// refreshed on every successful ChangeMessageVisibility — both the
	// auto-extend loop's own extends AND a user Extend (Finding 2). The
	// auto-extend loop reads it to decide whether a transient CMV failure
	// happened inside a still-valid window (retry) or after it lapsed
	// (cancel). Without refreshing it on user Extend, a user push to
	// now+10m would be ignored and a blip inside the old 30s window would
	// prematurely cancel still-locked processing.
	windowDeadline atomic.Int64
	logger         *slog.Logger
	metrics        ports.MetricsExporter
	clk            clock.Clock

	cancel           context.CancelFunc // cancels autoExtendCtx (stops the goroutine)
	processingCancel context.CancelFunc // cancels deliveryCtx (frees the context node)
	once             sync.Once          // ensures stopAutoExtend is idempotent
}

// newDelivery creates a delivery for a received SQS message.
//
// When autoExtend is true and visibilityTimeout > 1s, a background
// goroutine (autoExtendLoop) periodically calls ChangeMessageVisibility
// at one-third of visibilityTimeout. This keeps the message invisible to
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
	d.windowDeadline.Store(clk.Now().Add(time.Duration(visibilityTimeout) * time.Second).UnixNano())

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
//
// Before deleting, Ack guarantees the message still has a visibility margin
// larger than the delete's own timeout (Finding: c8-autoextend-margin) via
// ensureDeleteVisibilityMargin, so the delete always lands before the
// message could resurface to another consumer.
func (d *sqsDelivery) Ack(ctx context.Context) error {
	d.stopAutoExtend()
	defer d.cleanupContext()

	settleCtx, settleCancel := settlementContext(ctx)
	defer settleCancel()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: acking",
			"queue_url", d.queueURL,
			"message_id", d.env.ID(),
		)
	}

	// Guarantee the delete a visibility margin (Finding: c8-autoextend-
	// margin). Auto-extend is already stopped above, so this final extension
	// cannot race the background loop. It is passed the CALLER ctx (not
	// settleCtx): the margin CMV bounds itself independently so it can never
	// consume the delete's settlement budget.
	d.ensureDeleteVisibilityMargin(ctx)

	start := d.clk.Now()
	_, err := d.client.DeleteMessage(settleCtx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(d.queueURL),
		ReceiptHandle: aws.String(d.receiptHandle),
	})
	if err != nil {
		d.metrics.Counter(MetricSQSSettlementErrors, 1,
			shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
		return MapError(err)
	}
	d.metrics.Timer(MetricSQSDeleteLatency, d.clk.Since(start),
		shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
	return nil
}

// ensureDeleteVisibilityMargin extends the message visibility to
// ackDeleteMarginSeconds when the live window would otherwise lapse before
// DeleteMessage can complete (Finding: c8-autoextend-margin). DeleteMessage
// is bounded by sqsSettlementTimeout; a shorter remaining window could let
// the message resurface to another consumer mid-delete → duplicate
// processing. It is a no-op on the common path (a comfortably-large window
// pays no extra call) and best effort: a failed or timed-out extension just
// proceeds to the delete with the current window — no worse than before the
// fix — and is deliberately NOT counted as a settlement error, since the
// delete itself is what settles the message.
//
// ctx is the CALLER's Ack context. The CMV bounds ITSELF with marginCMVTimeout
// via boundedSettleContext — a separate, short budget from the delete's
// sqsSettlementTimeout — so a slow CMV can never starve the delete and turn a
// would-have-succeeded delete into a 15s-delayed redelivery.
func (d *sqsDelivery) ensureDeleteVisibilityMargin(ctx context.Context) {
	deadline := time.Unix(0, d.windowDeadline.Load())
	if deadline.Sub(d.clk.Now()) >= time.Duration(ackDeleteMarginSeconds)*time.Second {
		return
	}

	cmvCtx, cancel := boundedSettleContext(ctx, marginCMVTimeout)
	defer cancel()

	_, err := d.client.ChangeMessageVisibility(cmvCtx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(d.queueURL),
		ReceiptHandle:     aws.String(d.receiptHandle),
		VisibilityTimeout: ackDeleteMarginSeconds,
	})
	if err != nil {
		if d.logger != nil {
			d.logger.Warn("sqs: pre-delete visibility extension failed; proceeding to delete",
				"queue", d.queueURL,
				"message_id", d.env.ID(),
				"error", err,
			)
		}
		return
	}
	d.windowDeadline.Store(d.clk.Now().Add(time.Duration(ackDeleteMarginSeconds) * time.Second).UnixNano())
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
			"message_id", d.env.ID(),
			"delay_seconds", timeout,
		)
	}

	_, err := d.client.ChangeMessageVisibility(settleCtx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(d.queueURL),
		ReceiptHandle:     aws.String(d.receiptHandle),
		VisibilityTimeout: timeout,
	})
	if err != nil {
		d.metrics.Counter(MetricSQSSettlementErrors, 1,
			shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})
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
			"message_id", d.env.ID(),
			"new_timeout", timeout,
		)
	}

	// Bound the CMV call unconditionally (Finding 5): a hung connection
	// must not wedge the caller for the TCP RTO.
	callCtx, callCancel := settlementContext(ctx)
	defer callCancel()

	_, err := d.client.ChangeMessageVisibility(callCtx, &sqs.ChangeMessageVisibilityInput{
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
	// Refresh the auto-extend loop's window deadline so a transient CMV
	// failure inside the freshly-extended window is not mistaken for a
	// lapsed window and does not prematurely cancel processing (Finding 2).
	d.windowDeadline.Store(d.clk.Now().Add(time.Duration(timeout) * time.Second).UnixNano())
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
// one-third of the visibility timeout (the "tick interval"). Each call
// resets the invisibility clock to the full timeout value, giving the
// processor another full window to finish.
//
//	visibility = 30s → tick at 10s → extends to 30s from now → repeat
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
	vis := d.visibilityTimeout.Load()
	interval := autoExtendInterval(vis)

	ticker := d.clk.NewTicker(interval)
	defer ticker.Stop()

	// The CURRENT visibility window's lapse instant lives in
	// d.windowDeadline (unix nanos), seeded by newDelivery and refreshed on
	// every successful extend — the loop's own (below) AND a user Extend
	// (Finding 2). Reading it each failure lets a transient error retry
	// until (but not past) the true expiry instead of relying solely on a
	// fixed failure count; reading the atomic (not a loop-local) means a
	// user push to now+10m is honoured instead of ignored, so a blip inside
	// the old window can no longer prematurely cancel still-locked work.

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			vis = d.visibilityTimeout.Load()
			// Bound the CMV call unconditionally (Finding 5): a hung
			// connection must increment the failure counter within
			// sqsSettlementTimeout, not stall the loop for the TCP RTO.
			callCtx, callCancel := context.WithTimeout(ctx, sqsSettlementTimeout)
			_, err := d.client.ChangeMessageVisibility(callCtx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(d.queueURL),
				ReceiptHandle:     aws.String(d.receiptHandle),
				VisibilityTimeout: vis,
			})
			callCancel()
			now := d.clk.Now()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				d.metrics.Counter(MetricSQSAutoExtendFailures, 1,
					shared.Tag{Key: TagKeyQueueURL, Value: d.queueURL})

				// A receipt handle that is no longer inflight or is invalid
				// means the lock is already lost — the message is (or is
				// about to be) visible to another consumer. Retrying is
				// pointless and only widens the duplicate window, so cancel
				// processing immediately instead of treating it as transient
				// (Finding 8).
				var notInflight *sqstypes.MessageNotInflight
				var invalidHandle *sqstypes.ReceiptHandleIsInvalid
				if errors.As(err, &notInflight) || errors.As(err, &invalidHandle) {
					if d.logger != nil {
						d.logger.Error("sqs: auto-extend receipt handle no longer valid, cancelling processing",
							"queue", d.queueURL,
							"message_id", d.env.ID(),
							"error", err,
						)
					}
					if d.processingCancel != nil {
						d.processingCancel()
					}
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
				// Cancel processing when the window has actually lapsed
				// (the message may already be visible to another consumer)
				// OR after the defensive consecutive-failure ceiling. The
				// windowLapsed check is a small-visibility backstop: for
				// vis>=3 the failure ceiling (autoExtendMaxFailures) is
				// reached at or before the deadline, so it leads; only at
				// the minimum visibility (vis==2, interval floored to 1s)
				// does windowLapsed fire first, cancelling the instant the
				// lock is genuinely lost. The deadline is read from the
				// atomic so a user Extend (Finding 2) pushes it out.
				deadline := time.Unix(0, d.windowDeadline.Load())
				windowLapsed := !now.Before(deadline)
				if windowLapsed || consecutiveFailures >= autoExtendMaxFailures {
					if d.logger != nil {
						d.logger.Error("sqs: auto-extend giving up, cancelling processing",
							"queue", d.queueURL,
							"message_id", d.env.ID(),
							"consecutive_failures", consecutiveFailures,
							"window_lapsed", windowLapsed,
						)
					}
					if d.processingCancel != nil {
						d.processingCancel()
					}
					return
				}
				// Retry with deadline tracking. On the FIRST failure the
				// retry interval equals the regular cadence — retry =
				// (vis - vis/3)/2 = vis/3 = interval — so the ticker is
				// left unchanged. From the SECOND failure on the remaining
				// window shrinks and the retry (half the remaining window,
				// floored at 1s) becomes shorter than the cadence, so the
				// ticker is Reset to give several tries before the window
				// lapses.
				retry := autoExtendRetryInterval(deadline.Sub(now))
				if retry != interval {
					ticker.Reset(retry)
					interval = retry
				}
				continue
			}
			consecutiveFailures = 0
			// A successful extend resets the visibility window to `vis`
			// from now; publish the new deadline (Finding 2) and restore the
			// normal cadence.
			d.windowDeadline.Store(now.Add(time.Duration(vis) * time.Second).UnixNano())
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
					"message_id", d.env.ID(),
					"visibility_timeout", vis,
				)
			}
		}
	}
}

// autoExtendRetryInterval derives the retry cadence after a failed
// auto-extend: half the remaining window to the visibility deadline,
// floored at 1s so the clock Ticker never receives a non-positive
// duration. Retrying at half the remaining window packs several attempts
// in before the lock is lost while never busy-looping (Finding 5).
func autoExtendRetryInterval(remaining time.Duration) time.Duration {
	retry := remaining / 2
	if retry < time.Second {
		return time.Second
	}
	return retry
}
