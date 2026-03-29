package paho

import (
	"context"
	"log/slog"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/bridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Receiver implements ports.Receiver for MQTT subscriptions.
// It registers a handler with the Session's router and converts incoming
// MQTT publishes to Delivery values, calling the emit callback
// synchronously to apply backpressure. Messages are only dropped when
// the context is cancelled (shutdown).
type Receiver struct {
	id      string
	session *Session
	logger  *slog.Logger
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
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	logging.DebugContext(r.logger, ctx, "mqtt: receiver starting",
		"receiver_id", r.id)

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
			logging.DebugContext(r.logger, runCtx, "mqtt: emit error",
				"receiver_id", r.id, "error", err)
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
			logging.DebugContext(r.logger, ctx, "mqtt: receiver stopped",
				"receiver_id", r.id, "reason", "emit_error")
			return err
		default:
			logging.DebugContext(r.logger, ctx, "mqtt: receiver stopped",
				"receiver_id", r.id, "reason", "context_cancelled")
			return ctx.Err()
		}
	case err := <-errCh:
		logging.DebugContext(r.logger, ctx, "mqtt: receiver stopped",
			"receiver_id", r.id, "reason", "emit_error")
		return err
	}
}
