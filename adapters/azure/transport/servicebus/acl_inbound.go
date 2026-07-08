package servicebus

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
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
	// Stored as a plain int (not the SDK's uint32) so the runtime's
	// receive-count extraction — whose type switch covers int/int64/
	// float64/string — can read it once it learns the asb.delivery-count
	// key. See the SHARED-DEFERRED note for runtime/route/dispatch.go.
	//
	// The value is the EFFECTIVE receive count: broker DeliveryCount of
	// the current wire message plus the accumulated bridge retry counter
	// (x-bridge.retry-attempt) carried over from previous scheduled
	// copies. A delayed Retry schedules a fresh message whose broker
	// DeliveryCount restarts at 1; without the summation the runtime's
	// MaxReplayAttempts gate would never fire for retried messages.
	h[asbHeaderDeliveryCount] = effectiveReceiveCount(msg)

	if msg.CorrelationID != nil {
		h[asbHeaderCorrelationID] = *msg.CorrelationID
	}
	if msg.SessionID != nil {
		h[asbHeaderSessionID] = *msg.SessionID
	}
	if msg.PartitionKey != nil {
		h[asbHeaderPartitionKey] = *msg.PartitionKey
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

// bridgeAttempts extracts the accumulated bridge retry counter stamped
// by a delayed Retry (asbPropRetryAttempt) from the wire application
// properties. Returns 0 when absent, non-numeric, or negative. The
// AMQP round-trip may hand the value back in several numeric widths,
// so all common ones are accepted.
//
// Spoofing note: the property lives in the reserved x-bridge.*
// namespace, which ingress strips from envelope headers, and a forged
// LARGE value only makes the message DLQ earlier — fail-safe.
func bridgeAttempts(props map[string]any) int {
	v, ok := props[asbPropRetryAttempt]
	if !ok {
		return 0
	}
	n := 0
	switch t := v.(type) {
	case int:
		n = t
	case int32:
		n = int(t)
	case int64:
		n = int(t)
	case uint32:
		n = int(t)
	case uint64:
		n = int(t)
	case float64:
		n = int(t)
	case string:
		parsed, err := strconv.Atoi(t)
		if err != nil {
			return 0
		}
		n = parsed
	}
	if n < 0 {
		return 0
	}
	return n
}

// effectiveReceiveCount is the total 1-based number of times the
// LOGICAL message has been received: the broker DeliveryCount of the
// current wire message plus the counter carried over from previous
// bridge-scheduled retry copies.
func effectiveReceiveCount(msg *azservicebus.ReceivedMessage) int {
	return int(msg.DeliveryCount) + bridgeAttempts(msg.ApplicationProperties)
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
	if orig, ok := stringProp(msg.ApplicationProperties, asbPropOriginalMessageID); ok {
		// A bridge-scheduled retry copy salts its wire MessageID (so
		// broker duplicate detection never discards the copy) and
		// preserves the FIRST delivery's ID in this reserved property.
		// The envelope keeps the original ID so end-to-end idempotency
		// and dedup see one logical message across retries.
		id = orig
	}
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
//
// # Context hierarchy (mirrors the SQS adapter)
//
// The receiver's poll loop builds a three-level context tree per message:
//
//		pollLoop ctx (caller-owned)
//		  └─ deliveryCtx (WithCancel) — passed to emit() and to newDelivery
//		       └─ autoExtendCtx (WithCancel) — scoped to the auto-extend goroutine
//
//	  - cancel cancels autoExtendCtx (stops the goroutine) via stop(), called
//	    before the settlement API call in Ack/Retry.
//	  - processingCancel cancels deliveryCtx (the ctx handed to emit) via
//	    cleanupContext() after settlement, and is also invoked by the
//	    auto-extend loop on repeated lock-renewal failure so the in-flight
//	    emit callback observes a cancelled ctx and aborts processing.
type asbDelivery struct {
	env       *messaging.Envelope
	client    asbAPI
	scheduler retryScheduler
	msg       *azservicebus.ReceivedMessage
	logger    *slog.Logger
	metrics   ports.MetricsExporter
	clk       clock.Clock

	// receiveAndDelete marks a delivery received in
	// ReceiveModeReceiveAndDelete: the broker already removed the message
	// at receive time, so there is no lock to settle. Ack/Extend become
	// no-ops and Retry reports ErrNotSupported so the runtime DLQ-routes
	// rather than silently dropping. Set by the receiver before emit and
	// read only during settlement (never by the auto-extend goroutine,
	// which does not start in this mode), so it needs no synchronisation.
	receiveAndDelete bool

	// delayedRetryDisabled marks a delivery whose receiver structurally
	// cannot self-schedule a delayed retry — a topic subscription, where
	// scheduling targets the topic and would fan out to sibling
	// subscriptions. When true a delayed Retry falls back to
	// AbandonMessage and logs at debug (the absent scheduler is by design,
	// not a failure). Set by the receiver before emit; read only during
	// settlement.
	delayedRetryDisabled bool

	cancel           context.CancelFunc // cancels autoExtendCtx (stops the goroutine)
	processingCancel context.CancelFunc // cancels deliveryCtx (the ctx handed to emit)
	once             sync.Once          // makes stop() idempotent

	// renewalDeadline caps total lock auto-renewal wall time (zero =
	// no cap). Computed once in newDelivery from
	// deliveryTuning.maxLockRenewal; read only by the auto-extend
	// goroutine.
	renewalDeadline time.Time
}

// deliveryTuning groups the per-delivery lock-management knobs handed
// from ReceiverConfig to newDelivery.
type deliveryTuning struct {
	// lockDuration seeds the auto-extend cadence when the message
	// carries no LockedUntil (see newDelivery).
	lockDuration time.Duration
	// autoExtend starts the background lock-renewal goroutine.
	autoExtend bool
	// minAutoExtendInterval floors the computed renewal cadence
	// (default 1s when zero).
	minAutoExtendInterval time.Duration
	// maxLockRenewal caps total renewal wall time per delivery; when
	// exceeded processing is cancelled and renewal stops so a hung
	// pipeline cannot hold the message invisible forever. Zero = no
	// cap (tests); production configs default to 5m via applyDefaults.
	maxLockRenewal time.Duration
}

// newDelivery constructs an asbDelivery. clk drives the auto-extend
// (lock renewal) ticker; when nil it defaults to clock.System. Tests
// pass a clocktest.Fake to control tick firing deterministically.
//
// processingCancel is the cancel func for deliveryCtx (see the context
// hierarchy on asbDelivery). It may be nil in tests.
func newDelivery(
	parentCtx context.Context,
	env *messaging.Envelope,
	client asbAPI,
	scheduler retryScheduler,
	msg *azservicebus.ReceivedMessage,
	tuning deliveryTuning,
	processingCancel context.CancelFunc,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
	clk clock.Clock,
) *asbDelivery {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	if clk == nil {
		clk = clock.System
	}

	ctx, cancel := context.WithCancel(parentCtx)

	d := &asbDelivery{
		env:              env,
		client:           client,
		scheduler:        scheduler,
		msg:              msg,
		logger:           logger,
		metrics:          metrics,
		clk:              clk,
		cancel:           cancel,
		processingCancel: processingCancel,
	}
	if tuning.maxLockRenewal > 0 {
		d.renewalDeadline = clk.Now().Add(tuning.maxLockRenewal)
	}

	if tuning.autoExtend && tuning.lockDuration > 0 {
		// Renewal starts for any positive lock and derives its cadence from
		// the broker's real deadline: LockedUntil (peek-lock always carries
		// it) governs, so renewal self-corrects to the entity's true lock and
		// always fires before expiry. The lockDuration/2 seed below only
		// applies on the no-LockedUntil fallback (mock/defensive path). This
		// is why ASB needs no SQS-style below-floor guard: there is no fixed
		// short window here — cf. sqs Config.AutoExtendEnabled.
		interval := tuning.lockDuration / 2

		if msg.LockedUntil != nil && msg.LockedUntil.After(clk.Now()) {
			remaining := msg.LockedUntil.Sub(clk.Now())
			interval = remaining / 2
		}

		floor := time.Second
		if tuning.minAutoExtendInterval > 0 {
			floor = tuning.minAutoExtendInterval
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
	defer d.cleanupContext()

	if d.receiveAndDelete {
		// ReceiveAndDelete pre-settles at the broker: the message was
		// removed at receive time, so there is nothing to complete.
		// Treat as a successful no-op.
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "servicebus: ack no-op (receive-and-delete)",
				"message_id", d.msg.MessageID,
			)
		}
		return nil
	}

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
	defer d.cleanupContext()

	if d.receiveAndDelete {
		// ReceiveAndDelete pre-settles at the broker: the message is gone
		// and cannot be made available again. Report ErrNotSupported so the
		// runtime falls back to DLQ routing (no silent loss) instead of
		// looping on a retry that can never take effect.
		return shared.ErrNotSupported.WithMessage(
			"servicebus: Retry unavailable in ReceiveAndDelete mode (message already settled at receive)")
	}

	if after > 0 && d.scheduler == nil {
		if d.delayedRetryDisabled {
			// Topic subscription: delayed retry is structurally disabled
			// because a scheduled message targets the topic and would fan
			// out to every sibling subscription. Falling back to abandon
			// (same-subscription redelivery) is the expected, safe path —
			// log at debug, not error.
			if logging.DebugEnabled(d.logger) {
				d.logger.Log(ctx, logging.LevelDebug,
					"servicebus: delayed retry disabled for topic subscription (avoids fan-out); abandoning for same-subscription redelivery",
					"message_id", d.msg.MessageID,
					"requested_delay", after,
				)
			}
		} else if d.logger != nil {
			d.logger.Error("servicebus: Retry delay requested but no scheduler available, falling back to immediate abandon",
				"message_id", d.msg.MessageID,
				"requested_delay", after,
			)
		}
	}

	if after > 0 && d.scheduler != nil {
		if logging.TraceEnabled(d.logger) {
			d.logger.Log(ctx, logging.LevelTrace, "servicebus: scheduling retry",
				"message_id", d.msg.MessageID,
				"delay", after,
			)
		}

		newMsg := buildRetryMessage(d.msg, d.clk)

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
	if d.receiveAndDelete {
		// No lock exists in ReceiveAndDelete mode; nothing to renew.
		return nil
	}

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

// cleanupContext cancels deliveryCtx, freeing the per-message context
// node the receiver's poll loop allocated. Called via defer after the
// settlement API call in Ack/Retry — never before, because the caller
// passes deliveryCtx as the ctx argument and an early cancel would fail
// the settlement request with "context canceled".
func (d *asbDelivery) cleanupContext() {
	if d.processingCancel != nil {
		d.processingCancel()
	}
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
			if !d.renewalDeadline.IsZero() && !d.clk.Now().Before(d.renewalDeadline) {
				// Wall-time cap: a hung pipeline must not hold the message
				// locked (invisible, never redelivered, never DLQ'd)
				// indefinitely. Stop renewing and cancel processing; the
				// lock lapses and the broker redelivers or dead-letters
				// per MaxDeliveryCount.
				d.metrics.Counter(MetricASBLockRenewalCapExceeded, 1)
				if d.logger != nil {
					d.logger.Error("servicebus: max lock renewal duration exceeded, cancelling processing",
						"message_id", d.msg.MessageID,
					)
				}
				if d.processingCancel != nil {
					d.processingCancel()
				}
				return
			}
			if err := d.client.RenewMessageLock(ctx, d.msg, nil); err != nil {
				if ctx.Err() != nil {
					return
				}
				consecutiveFailures++
				d.metrics.Counter(MetricASBLockRenewalFailures, 1)
				if d.logger != nil {
					d.logger.Warn("servicebus: auto-extend lock failed",
						"message_id", d.msg.MessageID,
						"error", err,
						"consecutive_failures", consecutiveFailures,
					)
				}
				if consecutiveFailures >= autoExtendMaxFailures {
					d.metrics.Counter(MetricASBLockRenewerStopped, 1,
						shared.Tag{Key: asbTagKeyRenewerScope, Value: asbRenewerScopeDelivery})
					if d.logger != nil {
						d.logger.Error("servicebus: auto-extend max failures reached, cancelling processing",
							"message_id", d.msg.MessageID,
							"consecutive_failures", consecutiveFailures,
						)
					}
					// Cancel deliveryCtx (the ctx handed to emit) so the
					// in-flight processing pipeline observes cancellation and
					// aborts. The lock can no longer be held, so the broker
					// will redeliver — cancelling only the private
					// auto-extend ctx (d.cancel) would leave processing
					// running against a message whose lock has lapsed.
					if d.processingCancel != nil {
						d.processingCancel()
					}
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

// rawInbound is a non-SDK tuple returned by pollAndConvert: it bundles
// the translated envelope with the originating SDK message so the poll
// loop in receiver.go can hand both to newDelivery without ever naming
// an SDK type in its own imports (the asbDelivery settlement methods
// need the whole *azservicebus.ReceivedMessage, not just an id). It
// also pins the asbAPI client the message was received on: settlement
// must target the SAME link even if a credential rotation swaps the
// receiver stack between receive and settle.
type rawInbound struct {
	env    *messaging.Envelope
	msg    *azservicebus.ReceivedMessage
	client asbAPI
}

// pollAndConvert performs a single ReceiveMessages call against the
// asbAPI seam and converts each SDK message into a messaging.Envelope.
// All SDK types stay inside this ACL file; the SDK-free Receiver poll
// loop consumes only []rawInbound.
//
// max_wait_time (finding 7): when configured, the receive is bounded by
// a per-call deadline. ReceiveMessages blocks until messages arrive or
// the context is done, so the deadline is how an idle long-poll returns.
// A deadline that fires while the parent context is still live is a
// normal empty poll — pollAndConvert returns (nil, nil) so the loop
// re-issues immediately without treating it as a transport error.
func (r *Receiver) pollAndConvert(ctx context.Context) ([]rawInbound, error) {
	recvCtx := ctx
	if r.cfg.MaxWaitTime > 0 {
		var cancel context.CancelFunc
		recvCtx, cancel = context.WithTimeout(ctx, r.cfg.MaxWaitTime)
		defer cancel()
	}

	// Snapshot the client under the lock: ApplyCredentials may swap the
	// receiver stack concurrently (credential rotation). The snapshot is
	// used for the whole poll+abandon cycle so a mid-cycle swap can
	// never yield a nil or half-initialised client.
	client := r.currentClient()
	if client == nil {
		return nil, shared.ErrUnavailable.WithMessage("servicebus: receiver not started")
	}

	msgs, err := client.ReceiveMessages(recvCtx, r.cfg.MaxMessages, nil)
	if err != nil {
		// Our per-receive max_wait_time deadline fired (parent still
		// live): an idle long-poll, not a transport failure.
		if ctx.Err() == nil && recvCtx.Err() != nil {
			return nil, nil
		}
		return nil, err
	}

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelTrace, "servicebus: received",
			"entity", r.entityName(),
			"count", len(msgs),
		)
	}

	out := make([]rawInbound, 0, len(msgs))
	for _, msg := range msgs {
		env, convErr := receivedToEnvelope(msg, r.clock())
		if convErr != nil {
			// Release the lock so the broker can redeliver; after
			// MaxDeliveryCount abandons it dead-letters the poison message.
			// We deliberately do not fail the poll: a single poison message
			// must not stall the receive loop. In ReceiveAndDelete mode the
			// broker already removed the message at receive time, so there
			// is no lock to settle — skip the abandon, which would only
			// error.
			r.metrics.Counter(MetricASBMalformedMessages, 1)
			if r.logger != nil {
				r.logger.Warn("servicebus: dropping malformed delivery",
					"entity", r.entityName(),
					"message_id", msg.MessageID,
					"error", convErr,
				)
			}
			if !r.cfg.receiveAndDelete() {
				if abandonErr := client.AbandonMessage(ctx, msg, nil); abandonErr != nil && r.logger != nil {
					// Non-fatal: the lock will lapse and the broker
					// redelivers anyway — but surface it, a silent
					// abandon failure hides settlement problems.
					r.logger.Warn("servicebus: abandon of malformed delivery failed; lock will lapse",
						"entity", r.entityName(),
						"message_id", msg.MessageID,
						"error", abandonErr,
					)
				}
			}
			continue
		}
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelTrace, "servicebus: converting",
				"entity", r.entityName(),
				"message_id", msg.MessageID,
				"body_len", len(msg.Body),
			)
		}
		out = append(out, rawInbound{env: env, msg: msg, client: client})
	}
	return out, nil
}
