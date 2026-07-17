package paho

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

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
		if ack != nil {
			deliveryOpts = append(deliveryOpts, WithRetryFunc(r.session.requestRecovery))
		}
		del := NewDelivery(env, deliveryOpts...)
		if err := emit(runCtx, del); err != nil {
			// The delivery whose emit failed is left UN-ACKED, and MQTT
			// brokers do not redeliver on a live connection — without
			// intervention it (and any sibling un-acked deliveries) would
			// pin broker Receive-Maximum slots until an unrelated
			// connection teardown, wedging ingress as slots accumulate
			// (MQTT-L3). For durable QoS 1/2 (ack != nil), request the same
			// bounded, rate-limited session recycle a Delivery.Retry uses:
			// the resumed session redelivers every unsettled delivery
			// (duplicates absorbed downstream, the documented at-least-once
			// residual). Ephemeral sessions and QoS 0 have no redelivery
			// contract to recover — requestRecovery is not applicable and
			// the un-acked state dies with the connection.
			if ack != nil {
				if r.logger != nil {
					r.logger.Warn("mqtt: emit error stranded an un-acked delivery; "+
						"requesting bounded session recovery so the broker redelivers it",
						"receiver_id", r.id, "error", err)
				}
				if recErr := r.session.requestRecovery(runCtx); recErr != nil {
					logging.Debug(r.logger, "mqtt: session recovery request after emit error failed",
						"receiver_id", r.id, "error", recErr)
				}
			} else if logging.DebugEnabled(r.logger) {
				r.logger.Log(runCtx, logging.LevelDebug, "mqtt: emit error",
					"receiver_id", r.id, "error", err)
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
