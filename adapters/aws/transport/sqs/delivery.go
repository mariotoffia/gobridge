package sqs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

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
	visibilityTimeout int32
	logger            *slog.Logger

	cancel context.CancelFunc // stops auto-extend goroutine
	once   sync.Once          // ensures cancel runs exactly once
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
	logger *slog.Logger,
) *sqsDelivery {
	ctx, cancel := context.WithCancel(parentCtx)

	d := &sqsDelivery{
		env:               env,
		client:            client,
		queueURL:          queueURL,
		receiptHandle:     receiptHandle,
		visibilityTimeout: visibilityTimeout,
		logger:            logger,
		cancel:            cancel,
	}

	if autoExtend && visibilityTimeout > 1 {
		go d.autoExtendLoop(ctx)
	}

	return d
}

func (d *sqsDelivery) Envelope() *domain.Envelope { return d.env }

// Ack deletes the SQS message, confirming successful processing.
func (d *sqsDelivery) Ack(ctx context.Context) error {
	d.stop()

	_, err := d.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(d.queueURL),
		ReceiptHandle: aws.String(d.receiptHandle),
	})
	if err != nil {
		return MapError(err)
	}
	return nil
}

// Retry makes the message visible again after the given delay.
// A zero delay makes the message immediately available for re-delivery.
func (d *sqsDelivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.stop()

	timeout := int32(after.Seconds())
	if timeout < 0 {
		timeout = 0
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
func (d *sqsDelivery) Extend(ctx context.Context, until time.Time) error {
	timeout := int32(time.Until(until).Seconds())
	if timeout < 0 {
		timeout = 0
	}
	const sqsMaxVisibility = 43200
	if timeout > sqsMaxVisibility {
		timeout = sqsMaxVisibility
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

// stop cancels the auto-extend goroutine (idempotent).
func (d *sqsDelivery) stop() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
	})
}

// autoExtendLoop extends visibility at 50 % of the configured timeout
// until the context is cancelled (via Ack, Retry, or parent shutdown).
func (d *sqsDelivery) autoExtendLoop(ctx context.Context) {
	interval := time.Duration(d.visibilityTimeout) * time.Second / 2

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := d.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
				QueueUrl:          aws.String(d.queueURL),
				ReceiptHandle:     aws.String(d.receiptHandle),
				VisibilityTimeout: d.visibilityTimeout,
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if d.logger != nil {
					d.logger.Warn("sqs: auto-extend visibility failed",
						"queue", d.queueURL,
						"error", err,
					)
				}
				return
			}
		}
	}
}
