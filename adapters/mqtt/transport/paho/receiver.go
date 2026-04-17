package paho

import (
	"context"
	"log/slog"
	"sync/atomic"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Receiver implements ports.Receiver for MQTT subscriptions.
// It registers a handler with the Session's router and converts incoming
// MQTT publishes to Delivery values, calling the emit callback
// synchronously to apply backpressure. Messages are only dropped when
// the context is cancelled (shutdown).
//
// A Receiver is single-flight per instance: only one Run may be active
// at any time. A second concurrent Run on the same Receiver returns
// ErrUnavailable immediately and does NOT touch the router. This
// prevents handler theft caused by both Runs registering under the
// same id and the first Run's deferred Unregister removing the
// second Run's handler.
type Receiver struct {
	id      string
	session *Session
	logger  *slog.Logger
	running atomic.Bool // true while a Run is in flight
}

var _ ports.Receiver = (*Receiver)(nil)

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(id string, session *Session) *Receiver {
	return &Receiver{id: id, session: session, logger: session.logger}
}

// Run registers a message handler on the session router and blocks until
// ctx is cancelled or emit returns a non-nil error. Each incoming MQTT
// message is converted to a Delivery and passed to emit. The call to emit
// blocks, which naturally applies backpressure to the MQTT client.
//
// Concurrent Run calls on the same Receiver return ErrUnavailable; see
// the type-level documentation for the rationale.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	if !r.running.CompareAndSwap(false, true) {
		return domain.ErrUnavailable.WithMessage(
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

	r.session.Router().Register(r.id, func(pub *pahov5.Publish) {
		if logging.TraceEnabled(r.logger) {
			r.logger.Log(runCtx, logging.LevelTrace, "mqtt: message received",
				"receiver_id", r.id,
				"topic", pub.Topic,
				"payload_len", len(pub.Payload),
			)
		}

		env := EnvelopeFromPublish(pub)
		del := NewDelivery(env)
		if err := emit(runCtx, del); err != nil {
			if logging.DebugEnabled(r.logger) {
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
