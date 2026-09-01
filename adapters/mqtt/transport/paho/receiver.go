package paho

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Receiver implements ports.Receiver for MQTT subscriptions.
// It registers a handler with the Session's router and converts incoming
// MQTT publishes to Delivery values, calling the emit callback
// synchronously to apply backpressure. Messages are only dropped when
// the context is cancelled (shutdown).
//
// When constructed with topic filters (WithTopicFilters — the factory
// passes the route's subscription topics), the router dispatches only
// publishes whose topic matches one of the filters (MQTT wildcards
// supported), so multiple Receivers sharing one Session do not process
// each other's messages. Without filters the Receiver handles every
// publish on the session (legacy behaviour).
//
// Each Delivery carries the manual protocol-ack callback for its
// originating publish: the MQTT PUBACK/PUBCOMP is sent when the runtime
// settles the delivery via Delivery.Ack, not when emit returns (see
// delivery.go).
//
// A Receiver is single-flight per instance: only one Run may be active
// at any time. A second concurrent Run on the same Receiver returns
// ErrUnavailable immediately and does NOT touch the router. This
// prevents handler theft caused by both Runs registering under the
// same id and the first Run's deferred Unregister removing the
// second Run's handler.
type Receiver struct {
	id          string
	session     *Session
	logger      *slog.Logger
	filters     []string    // MQTT topic filters selecting this receiver's traffic; empty = all
	running     atomic.Bool // true while a Run is in flight
	started     chan struct{}
	startedOnce sync.Once
}

var _ ports.Receiver = (*Receiver)(nil)

// ReceiverOption customises a Receiver.
type ReceiverOption func(*Receiver)

// WithTopicFilters restricts the Receiver to publishes whose topic
// matches at least one of the given MQTT topic filters (`+`/`#`
// wildcards and $share prefixes supported). No filters means the
// Receiver handles every publish on the session.
func WithTopicFilters(filters ...string) ReceiverOption {
	return func(r *Receiver) { r.filters = append(r.filters, filters...) }
}

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(id string, session *Session, opts ...ReceiverOption) *Receiver {
	r := &Receiver{
		id:      id,
		session: session,
		logger:  session.logger,
		started: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Started returns a channel that is closed once the receiver has
// registered its router handler and is ready to process messages.
// It satisfies ports.ReceiverStartedSignaler.
func (r *Receiver) Started() <-chan struct{} { return r.started }

// Run registers a message handler on the session router and blocks until
// ctx is cancelled or emit returns a non-nil error. Each incoming MQTT
// message is converted to a Delivery and passed to emit. The call to emit
// blocks, which naturally applies backpressure to the MQTT client.
//
// Concurrent Run calls on the same Receiver return ErrUnavailable; see
// the type-level documentation for the rationale.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if !r.running.CompareAndSwap(false, true) {
		return shared.ErrUnavailable.WithMessage(
			"mqtt receiver " + r.id + ": Run already in flight; receivers are single-flight per instance")
	}
	defer r.running.Store(false)

	if logging.DebugEnabled(r.logger) {
		r.logger.Log(ctx, logging.LevelDebug, "mqtt: receiver starting",
			"receiver_id", r.id)
	}

	// Use a child context so we can cancel the handler when emit fails.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)

	r.session.Router().RegisterEnvelope(r.id, r.session.clock(), r.filters, func(env *messaging.Envelope, ack func() error) {
		if logging.TraceEnabled(r.logger) {
			var transportTopic string
			if env.Headers() != nil {
				if v, ok := env.Headers()[HeaderMQTTTopic].(string); ok {
					transportTopic = v
				}
			}
			r.logger.Log(runCtx, logging.LevelTrace, "mqtt: message received",
				"receiver_id", r.id,
				"topic", transportTopic,
				"payload_len", len(env.Payload()),
			)
		}

		deliveryOpts := []DeliveryOption{WithAckFunc(ack)}
		recoverable := ack != nil && r.session.mode != connectivity.SessionEphemeral
		if recoverable {
			deliveryOpts = append(deliveryOpts, WithRetryFunc(r.session.requestRecovery))
		}
		del := NewDelivery(env, deliveryOpts...)
		if err := emit(runCtx, del); err != nil {
			// A delivery whose emit failed is left UN-ACKED, and MQTT brokers do
			// not redeliver on a live connection — without intervention it (and
			// any sibling un-acked deliveries) would pin broker Receive-Maximum
			// slots until an unrelated connection teardown, wedging ingress as
			// slots accumulate. What can be done about it depends on whether the
			// broker session RESUMES:
			//
			//   - Durable QoS 1/2 on a persistent/exclusive session: request the
			//     same bounded, rate-limited recycle a Delivery.Retry uses. The
			//     resumed session redelivers every unsettled delivery
			//     (duplicates absorbed downstream, the documented at-least-once
			//     residual).
			//   - Durable QoS 1/2 on an EPHEMERAL session: clean_start=true
			//     discards the server-side session at the next disconnect, so
			//     nothing will ever be redelivered. Withholding the ack buys no
			//     recovery and pins a Receive-Maximum slot for the life of the
			//     connection, so the bounded policy is to ack, drop, and record
			//     the loss — the same accounting QoS 0 gets.
			//   - QoS 0: no acknowledgement to withhold, no redelivery contract.
			r.countEmitRejection(recoverable)
			switch {
			case recoverable:
				if r.logger != nil {
					r.logger.Warn("mqtt: emit error stranded an un-acked delivery; "+
						"requesting bounded session recovery so the broker redelivers it",
						"receiver_id", r.id, "error", err)
				}
				if recErr := r.session.requestRecovery(runCtx); recErr != nil {
					logging.Debug(r.logger, "mqtt: session recovery request after emit error failed",
						"receiver_id", r.id, "error", recErr)
				}
			case ack != nil:
				if r.logger != nil {
					r.logger.Warn("mqtt: emit error DROPPED a QoS 1/2 delivery on an ephemeral "+
						"session — clean_start=true leaves no session to resume, so no recycle can "+
						"redeliver it; acking frees the receive-window slot it would otherwise pin "+
						"until disconnect",
						"receiver_id", r.id, "error", err)
				}
				// Through the Delivery, never the raw callback: Ack is
				// once-guarded, so a runner that settled the delivery and THEN
				// failed cannot be double-acknowledged here.
				if ackErr := del.Ack(runCtx); ackErr != nil {
					logging.Debug(r.logger, "mqtt: ack of a dropped ephemeral delivery failed",
						"receiver_id", r.id, "error", ackErr)
				}
			default:
				if r.logger != nil {
					r.logger.Warn("mqtt: emit error DROPPED a QoS 0 delivery — there is no "+
						"acknowledgement to withhold and no redelivery contract, so the message is lost",
						"receiver_id", r.id, "error", err)
				}
			}
			// Signal the error and cancel so the handler unblocks.
			select {
			case errCh <- err:
			default:
			}
			runCancel()
		}
	})
	defer r.session.Router().Unregister(r.id)

	r.startedOnce.Do(func() { close(r.started) })

	select {
	case <-runCtx.Done():
		// Check if it was an emit error or parent cancellation.
		select {
		case err := <-errCh:
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "mqtt: receiver stopped",
					"receiver_id", r.id, "reason", "emit_error")
			}
			return err
		default:
			if logging.DebugEnabled(r.logger) {
				r.logger.Log(ctx, logging.LevelDebug, "mqtt: receiver stopped",
					"receiver_id", r.id, "reason", "context_cancelled")
			}
			return ctx.Err()
		}
	case err := <-errCh:
		if logging.DebugEnabled(r.logger) {
			r.logger.Log(ctx, logging.LevelDebug, "mqtt: receiver stopped",
				"receiver_id", r.id, "reason", "emit_error")
		}
		return err
	}
}

// Outcome tag values for MetricMQTTReceiverEmitRejected.
const (
	// emitRejectionRecovering marks a refused DURABLE delivery on a resuming
	// session: it stays un-acked and the bounded session recycle makes the
	// broker redeliver it.
	emitRejectionRecovering = "recovering"
	// emitRejectionLost marks a refused delivery nothing will redeliver: QoS 0
	// (no acknowledgement to withhold), or QoS 1/2 on an ephemeral session
	// (clean_start=true leaves no session to resume). Either way the message is
	// gone, and the acknowledgement is sent so it stops pinning the broker's
	// receive window.
	emitRejectionLost = "lost"
)

// countEmitRejection records one delivery the route pipeline refused at emit.
// Both outcomes are counted: the recoverable one because it predicts the
// session recycle that follows, and the lost one because nothing else would
// ever record that the message was gone. "Recoverable" is a durable
// acknowledgement on a session that RESUMES — a QoS 1/2 delivery on an
// ephemeral session is counted as lost, because clean_start=true means no
// recycle can bring it back.
func (r *Receiver) countEmitRejection(recoverable bool) {
	outcome := emitRejectionLost
	if recoverable {
		outcome = emitRejectionRecovering
	}
	r.session.metrics.Counter(MetricMQTTReceiverEmitRejected, 1,
		shared.Tag{Key: shared.TagKeySessionID, Value: r.session.opts.ClientID},
		shared.Tag{Key: shared.TagKeyOutcome, Value: outcome},
	)
}
