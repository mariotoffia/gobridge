package paho

import (
	"context"

	pahov5 "github.com/eclipse/paho.golang/paho"
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
}

var _ ports.Receiver = (*Receiver)(nil)

// NewReceiver creates a Receiver bound to the given Session.
func NewReceiver(id string, session *Session) *Receiver {
	return &Receiver{id: id, session: session}
}

// Run registers a message handler on the session router and blocks until
// ctx is cancelled or emit returns a non-nil error. Each incoming MQTT
// message is converted to a Delivery and passed to emit. The call to emit
// blocks, which naturally applies backpressure to the MQTT client.
func (r *Receiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	// Use a child context so we can cancel the handler when emit fails.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	errCh := make(chan error, 1)

	r.session.Router().Register(r.id, func(pub *pahov5.Publish) {
		env := EnvelopeFromPublish(pub)
		del := NewDelivery(env)
		if err := emit(runCtx, del); err != nil {
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
			return err
		default:
			return ctx.Err()
		}
	case err := <-errCh:
		return err
	}
}
