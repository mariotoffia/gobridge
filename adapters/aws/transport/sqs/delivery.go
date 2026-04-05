package sqs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time check.
var _ ports.Delivery = (*sqsDelivery)(nil)

// sqsDelivery wraps a received SQS message and maps Ack/Retry/Extend
// to the corresponding SQS receipt-handle operations.
type sqsDelivery struct {
	env               *domain.Envelope
	client            sqsAPI
	queueURL          string
	receiptHandle     string
	visibilityTimeout atomic.Int32
	logger            *slog.Logger
	metrics           ports.MetricsExporter

	cancel           context.CancelFunc // stops auto-extend goroutine
	processingCancel context.CancelFunc // cancels the processing goroutine on extend failure
	once             sync.Once          // ensures cancel runs exactly once
}

// newDelivery creates a delivery for a received SQS message.
// When autoExtend is true a background goroutine periodically extends
// visibility at 50 % of visibilityTimeout until the delivery is
// acknowledged, retried, or the parent context is cancelled.
func newDelivery(
	parentCtx context.Context,
	env *domain.Envelope,
	client sqsAPI,
	queueURL string,
	receiptHandle string,
	visibilityTimeout int32,
	autoExtend bool,
	processingCancel context.CancelFunc,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
) *sqsDelivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
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
		cancel:           cancel,
	}
	d.visibilityTimeout.Store(visibilityTimeout)

	if autoExtend && visibilityTimeout > 1 {
		go d.autoExtendLoop(ctx)
	}

	return d
}

func (d *sqsDelivery) Envelope() *domain.Envelope { return d.env }

// Ack deletes the SQS message, confirming successful processing.
func (d *sqsDelivery) Ack(ctx context.Context) error {
	d.stop()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: acking",
			"queue_url", d.queueURL,
			"message_id", d.env.ID,
		)
	}

	start := time.Now()
	_, err := d.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(d.queueURL),
		ReceiptHandle: aws.String(d.receiptHandle),
	})
	if err != nil {
		return MapError(err)
	}
	d.metrics.Timer(domain.MetricSQSDeleteLatency, time.Since(start),
		domain.Tag{Key: domain.TagKeyQueueURL, Value: d.queueURL})
	return nil
}

// Retry makes the message visible again after the given delay.
// A zero delay makes the message immediately available for re-delivery.
func (d *sqsDelivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.stop()

	timeout := int32(after.Seconds())
	if timeout < 0 {
		timeout = 0
	} else if timeout == 0 && after > 0 {
		timeout = 1
	}
	const sqsMaxVisibility = 43200
	if timeout > sqsMaxVisibility {
		timeout = sqsMaxVisibility
	}

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "sqs: retrying",
			"queue_url", d.queueURL,
			"message_id", d.env.ID,
			"delay_seconds", timeout,
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
	return nil
}

// Extend pushes the visibility timeout to the given absolute time.
// The delta from now is clamped to [0, 43200] seconds (SQS maximum).
// Also updates the stored timeout used by auto-extend to prevent it
// from resetting visibility to a shorter value on the next tick.
func (d *sqsDelivery) Extend(ctx context.Context, until time.Time) error {
	timeout := int32(time.Until(until).Seconds())
	if timeout < 0 {
		timeout = 0
	}
	const sqsMaxVisibility = 43200
	if timeout > sqsMaxVisibility {
		timeout = sqsMaxVisibility
	}

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

	d.metrics.Counter(domain.MetricSQSVisibilityExtensions, 1,
		domain.Tag{Key: domain.TagKeyQueueURL, Value: d.queueURL})
	d.visibilityTimeout.Store(timeout)
	return nil
}

// stop cancels the auto-extend goroutine (idempotent).
func (d *sqsDelivery) stop() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		// Also cancel the per-delivery context created by the receiver's
		// Run loop. Without this, each successfully-processed message
		// leaves a dangling context node in the Go runtime's context tree,
		// growing monotonically over the receiver's lifetime.
		if d.processingCancel != nil {
			d.processingCancel()
		}
	})
}

const autoExtendMaxFailures = 3

// autoExtendLoop extends visibility at 50 % of the configured timeout
// until the context is cancelled (via Ack, Retry, or parent shutdown).
// Tolerates up to autoExtendMaxFailures consecutive transient errors
// before giving up.
func (d *sqsDelivery) autoExtendLoop(ctx context.Context) {
	interval := time.Duration(d.visibilityTimeout.Load()) * time.Second / 2

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
			newInterval := time.Duration(vis) * time.Second / 2
			if newInterval != interval {
				ticker.Reset(newInterval)
				interval = newInterval
			}
			d.metrics.Counter(domain.MetricSQSAutoExtends, 1,
				domain.Tag{Key: domain.TagKeyQueueURL, Value: d.queueURL})
			logging.TraceContext(d.logger, ctx, "sqs: auto-extended",
				"queue_url", d.queueURL,
				"message_id", d.env.ID,
				"visibility_timeout", vis,
			)
		}
	}
}
