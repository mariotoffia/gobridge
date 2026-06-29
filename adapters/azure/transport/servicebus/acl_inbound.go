package servicebus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// messageToHeaders maps an incoming Service Bus message's system
// properties and application properties to an envelope header map.
// Reserved x-bridge.* headers carried in ApplicationProperties are
// stripped to prevent injection.
func messageToHeaders(msg *azservicebus.ReceivedMessage) map[string]any {
	h := make(map[string]any, len(msg.ApplicationProperties)+11)

	h[asbHeaderMessageID] = msg.MessageID
	h[asbHeaderDeliveryCount] = msg.DeliveryCount

	if msg.CorrelationID != nil {
		h[asbHeaderCorrelationID] = *msg.CorrelationID
	}
	if msg.SessionID != nil {
		h[asbHeaderSessionID] = *msg.SessionID
	}
	if msg.ContentType != nil {
		h[asbHeaderContentType] = *msg.ContentType
	}
	if msg.Subject != nil {
		h[asbHeaderSubject] = *msg.Subject
	}
	if msg.To != nil {
		h[asbHeaderTo] = *msg.To
	}
	if msg.ReplyTo != nil {
		h[asbHeaderReplyTo] = *msg.ReplyTo
	}
	if msg.TimeToLive != nil {
		h[asbHeaderTTL] = *msg.TimeToLive
	}
	if msg.EnqueuedTime != nil {
		h[asbHeaderEnqueuedTime] = *msg.EnqueuedTime
	}
	if msg.SequenceNumber != nil {
		h[asbHeaderSequenceNum] = *msg.SequenceNumber
	}

	for k, v := range msg.ApplicationProperties {
		if messaging.IsReservedHeader(k) {
			continue
		}
		h[k] = v
	}

	return h
}

// receivedToEnvelope translates an inbound *azservicebus.ReceivedMessage
// to a fresh messaging.Envelope. Envelope.Subject is populated solely
// from the native Service Bus Subject; the queue/topic entity name is
// NEVER promoted into Envelope.Subject. When the broker carries no
// native Subject, Envelope.Subject is left empty.
func receivedToEnvelope(msg *azservicebus.ReceivedMessage, clk clock.Clock) (*messaging.Envelope, error) {
	if clk == nil {
		clk = clock.System
	}
	subject := ""
	if msg.Subject != nil {
		subject = *msg.Subject
	}
	id := msg.MessageID
	if id == "" {
		id = generateEnvelopeID()
	}
	// A received broker absolute-expiry is stamped at construction (permissive):
	// a message that expired in transit is accepted as an already-expired
	// envelope and dropped downstream by the TTL/IsExpired logic rather than
	// failing ingress.
	var expiresAt time.Time
	if msg.ExpiresAt != nil {
		expiresAt = *msg.ExpiresAt
	}
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        id,
		Subject:   subject,
		Payload:   msg.Body,
		Headers:   messageToHeaders(msg),
		CreatedAt: clk.Now(),
		ExpiresAt: expiresAt,
	}, clk.Now())
	if err != nil {
		return nil, wrapEnvelopeErr(err)
	}
	return env, nil
}

var _ ports.Delivery = (*asbDelivery)(nil)

// asbDelivery wraps an inbound Azure Service Bus message and implements
// ports.Delivery. Settlement (Ack/Retry/Extend) is forwarded to the
// asbAPI seam; SDK-typed plumbing stays local to this ACL file.
type asbDelivery struct {
	env       *messaging.Envelope
	client    asbAPI
	scheduler retryScheduler
	msg       *azservicebus.ReceivedMessage
	logger    *slog.Logger
	metrics   ports.MetricsExporter
	clk       clock.Clock
	cancel    context.CancelFunc
	once      sync.Once
}

// newDelivery constructs an asbDelivery. clk drives the auto-extend
// (lock renewal) ticker; when nil it defaults to clock.System. Tests
// pass a clocktest.Fake to control tick firing deterministically.
func newDelivery(
	parentCtx context.Context,
	env *messaging.Envelope,
	client asbAPI,
	scheduler retryScheduler,
	msg *azservicebus.ReceivedMessage,
	lockDuration time.Duration,
	autoExtend bool,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
	minAutoExtendInterval ...time.Duration,
) *asbDelivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	if clk == nil {
		clk = clock.System
	}

	ctx, cancel := context.WithCancel(parentCtx)

	d := &asbDelivery{
		env:       env,
		client:    client,
		scheduler: scheduler,
		msg:       msg,
		logger:    logger,
		metrics:   metrics,
		clk:       clk,
		cancel:    cancel,
	}

	if autoExtend && lockDuration > 0 {
		interval := lockDuration / 2

		if msg.LockedUntil != nil && msg.LockedUntil.After(clk.Now()) {
			remaining := msg.LockedUntil.Sub(clk.Now())
			interval = remaining / 2
		}

		floor := time.Second
		if len(minAutoExtendInterval) > 0 && minAutoExtendInterval[0] > 0 {
			floor = minAutoExtendInterval[0]
		}
		if interval < floor {
			interval = floor
		}

		go d.autoExtendLoop(ctx, interval)
	}

	return d
}

func (d *asbDelivery) Envelope() *messaging.Envelope { return d.env }

func (d *asbDelivery) Ack(ctx context.Context) error {
	d.stop()

	if logging.TraceEnabled(d.logger) {
		d.logger.Log(ctx, logging.LevelTrace, "servicebus: completing",
			"message_id", d.msg.MessageID,
		)
	}

	start := d.clk.Now()
	if err := d.client.CompleteMessage(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}

	// MetricASBCompleteLatency is emitted here because the InstrumentedDelivery
	// wrapper uses the generic MetricAckLatency; this gives ASB-specific detail.
	d.metrics.Timer(MetricASBCompleteLatency, d.clk.Since(start))

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

		newMsg := buildRetryMessage(d.msg)

		enqueueAt := d.clk.Now().Add(after)
		schedStart := d.clk.Now()
		seqNums, err := d.scheduler.ScheduleMessages(ctx, []*azservicebus.Message{newMsg}, enqueueAt, nil)
		if err != nil {
			return MapError(err)
		}
		d.metrics.Timer(MetricASBScheduleLatency, d.clk.Since(schedStart))

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
	ticker := d.clk.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
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
					if d.logger != nil {
						d.logger.Error("servicebus: auto-extend max failures reached, cancelling processing",
							"message_id", d.msg.MessageID,
							"consecutive_failures", consecutiveFailures,
						)
					}
					d.cancel()
					return
				}
				continue
			}
			consecutiveFailures = 0
			d.metrics.Counter(MetricASBLockRenewals, 1)
			if logging.TraceEnabled(d.logger) {
				d.logger.Log(ctx, logging.LevelTrace, "servicebus: lock renewed",
					"message_id", d.msg.MessageID,
				)
			}
		}
	}
}

// receiveAndConvert performs a single ReceiveMessages call against the
// asbAPI seam, then converts each SDK message into an asbDelivery. It
// is the bridge between the SDK-typed pump and the SDK-free Receiver
// poll loop.
func (r *Receiver) receiveAndConvert(ctx context.Context) ([]ports.Delivery, error) {
	msgs, err := r.client.ReceiveMessages(ctx, r.cfg.MaxMessages, nil)
	if err != nil {
		return nil, err
	}
	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "servicebus: received",
			"entity", r.entityName(),
			"count", len(msgs),
		)
	}
	out := make([]ports.Delivery, 0, len(msgs))
	for _, msg := range msgs {
		d, err := r.toDelivery(ctx, msg)
		if err != nil {
			// Malformed inbound — abandon at the broker so it is not
			// redelivered tightly, then drop. We deliberately do not
			// fail the poll: a single poison message must not stall
			// the receive loop.
			r.metrics.Counter(MetricASBMalformedMessages, 1)
			if r.logger != nil {
				r.logger.Warn("servicebus: dropping malformed delivery",
					"entity", r.entityName(),
					"message_id", msg.MessageID,
					"error", err,
				)
			}
			_ = r.client.AbandonMessage(ctx, msg, nil)
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// toDelivery converts a single received SDK message into an
// asbDelivery, attaching the configured logger, metrics, and clock.
func (r *Receiver) toDelivery(ctx context.Context, msg *azservicebus.ReceivedMessage) (*asbDelivery, error) {
	env, err := receivedToEnvelope(msg, r.clock())
	if err != nil {
		return nil, err
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "servicebus: converting",
			"entity", r.entityName(),
			"message_id", msg.MessageID,
			"body_len", len(msg.Body),
		)
	}

	return newDelivery(
		ctx,
		env,
		r.client,
		r.scheduler,
		msg,
		r.cfg.LockDuration,
		r.cfg.autoExtendEnabled(),
		r.logger,
		r.metrics,
		r.clock(),
		r.cfg.MinAutoExtendInterval,
	), nil
}
