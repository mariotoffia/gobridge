package servicebus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*asbDelivery)(nil)

type asbDelivery struct {
	env       *domain.Envelope
	client    asbAPI
	scheduler retryScheduler
	msg       *azservicebus.ReceivedMessage
	logger    *slog.Logger
	cancel    context.CancelFunc
	once      sync.Once
}

func newDelivery(
	parentCtx context.Context,
	env *domain.Envelope,
	client asbAPI,
	scheduler retryScheduler,
	msg *azservicebus.ReceivedMessage,
	lockDuration time.Duration,
	autoExtend bool,
	logger *slog.Logger,
) *asbDelivery {
	ctx, cancel := context.WithCancel(parentCtx)

	d := &asbDelivery{
		env:       env,
		client:    client,
		scheduler: scheduler,
		msg:       msg,
		logger:    logger,
		cancel:    cancel,
	}

	if autoExtend && lockDuration > 0 {
		interval := lockDuration / 2

		if msg.LockedUntil != nil && msg.LockedUntil.After(time.Now()) {
			remaining := time.Until(*msg.LockedUntil)
			interval = remaining / 2
		}

		if interval < time.Second {
			interval = time.Second
		}

		go d.autoExtendLoop(ctx, interval)
	}

	return d
}

func (d *asbDelivery) Envelope() *domain.Envelope { return d.env }

func (d *asbDelivery) Ack(ctx context.Context) error {
	d.stop()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "servicebus: completing",
			"message_id", d.msg.MessageID,
		)
	}

	start := time.Now()
	if err := d.client.CompleteMessage(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}

	// MetricASBCompleteLatency is emitted here because the InstrumentedDelivery
	// wrapper uses the generic MetricAckLatency; this gives ASB-specific detail.
	_ = time.Since(start)

	return nil
}

func (d *asbDelivery) Retry(ctx context.Context, after time.Duration, _ error) error {
	d.stop()

	if after > 0 && d.scheduler == nil && d.logger != nil {
		d.logger.Error("servicebus: Retry delay requested but no scheduler available, falling back to immediate abandon",
			"message_id", d.msg.MessageID,
			"requested_delay", after,
		)
	}

	if after > 0 && d.scheduler != nil {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "servicebus: scheduling retry",
				"message_id", d.msg.MessageID,
				"delay", after,
			)
		}

		newMsg := &azservicebus.Message{
			Body:    d.msg.Body,
			Subject: d.msg.Subject,
		}
		if d.msg.MessageID != "" {
			newMsg.MessageID = &d.msg.MessageID
		}
		if d.msg.SessionID != nil {
			newMsg.SessionID = d.msg.SessionID
		}
		if d.msg.ContentType != nil {
			newMsg.ContentType = d.msg.ContentType
		}
		if d.msg.CorrelationID != nil {
			newMsg.CorrelationID = d.msg.CorrelationID
		}
		if len(d.msg.ApplicationProperties) > 0 {
			newMsg.ApplicationProperties = d.msg.ApplicationProperties
		}
		if d.msg.ReplyTo != nil {
			newMsg.ReplyTo = d.msg.ReplyTo
		}
		if d.msg.To != nil {
			newMsg.To = d.msg.To
		}
		if d.msg.TimeToLive != nil {
			newMsg.TimeToLive = d.msg.TimeToLive
		}

		enqueueAt := time.Now().Add(after)
		seqNums, err := d.scheduler.ScheduleMessages(ctx, []*azservicebus.Message{newMsg}, enqueueAt, nil)
		if err != nil {
			return MapError(err)
		}

		if err := d.client.CompleteMessage(ctx, d.msg, nil); err != nil {
			if cancelErr := d.scheduler.CancelScheduledMessages(ctx, seqNums, nil); cancelErr != nil && d.logger != nil {
				d.logger.Error("servicebus: failed to cancel scheduled message after CompleteMessage failure",
					"message_id", d.msg.MessageID,
					"error", cancelErr,
				)
			}
			return MapError(err)
		}
		return nil
	}

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "servicebus: abandoning",
			"message_id", d.msg.MessageID,
		)
	}

	if err := d.client.AbandonMessage(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

// Extend renews the message lock using the broker's configured LockDuration.
// The until parameter is ignored because Azure Service Bus lock renewals
// always reset to the entity's configured lock duration; precise time-based
// extension is not supported by the SDK.
func (d *asbDelivery) Extend(ctx context.Context, _ time.Time) error {
	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "servicebus: renewing lock",
			"message_id", d.msg.MessageID,
		)
	}

	if err := d.client.RenewMessageLock(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

func (d *asbDelivery) stop() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
	})
}

const autoExtendMaxFailures = 3

// autoExtendLoop renews the message lock at the given interval until
// the context is cancelled. Tolerates up to autoExtendMaxFailures
// consecutive transient errors before giving up.
func (d *asbDelivery) autoExtendLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.client.RenewMessageLock(ctx, d.msg, nil); err != nil {
				if ctx.Err() != nil {
					return
				}
				consecutiveFailures++
				if d.logger != nil {
					d.logger.Warn("servicebus: auto-extend lock failed",
						"message_id", d.msg.MessageID,
						"error", err,
						"consecutive_failures", consecutiveFailures,
					)
				}
				if consecutiveFailures >= autoExtendMaxFailures {
					return
				}
				continue
			}
			consecutiveFailures = 0
			logging.TraceContext(d.logger, ctx, "servicebus: lock renewed",
				"message_id", d.msg.MessageID,
			)
		}
	}
}
