package transport

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type deliveryResult struct {
	err error
}

// httpDelivery implements ports.Delivery for HTTP-originated messages.
// Ack and Retry signal the HTTP handler via a buffered channel.
type httpDelivery struct {
	env  *messaging.Envelope
	done chan deliveryResult
	// onSettle, when non-nil, runs exactly once on the first Ack or
	// Retry. The receiver wires the dispatch-deadline CancelFunc here so
	// the detached dispatch context (context.WithoutCancel + the
	// server's own deadline re-armed — see (*Receiver).ServeHTTP) is
	// released when the pipeline settles the delivery, not when the
	// handler returns.
	onSettle   func()
	settleOnce sync.Once
}

func newHTTPDelivery(env *messaging.Envelope) *httpDelivery {
	return &httpDelivery{
		env:  env,
		done: make(chan deliveryResult, 1),
	}
}

func (d *httpDelivery) Envelope() *messaging.Envelope { return d.env }

// settled runs the onSettle hook exactly once across Ack/Retry calls.
func (d *httpDelivery) settled() {
	d.settleOnce.Do(func() {
		if d.onSettle != nil {
			d.onSettle()
		}
	})
}

func (d *httpDelivery) Ack(_ context.Context) error {
	d.settled()
	select {
	case d.done <- deliveryResult{}:
	default:
	}
	return nil
}

func (d *httpDelivery) Retry(_ context.Context, _ time.Duration, reason error) error {
	// A nil reason must still be distinguishable from Ack: the handler
	// keys success on result.err == nil, so a Retry(nil) would otherwise
	// masquerade as a successful delivery (200 to the producer for a
	// message the pipeline did NOT process). Substitute a generic
	// transient reason.
	if reason == nil {
		reason = shared.ErrUnavailable.WithMessage("http: delivery retry requested without reason")
	}
	d.settled()
	select {
	case d.done <- deliveryResult{err: reason}:
	default:
	}
	return nil
}

func (d *httpDelivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
