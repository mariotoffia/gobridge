package transport

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type deliveryResult struct {
	err error
}

// httpDelivery implements ports.Delivery for HTTP-originated messages.
// Ack and Retry signal the HTTP handler via a buffered channel.
type httpDelivery struct {
	env  *domain.Envelope
	done chan deliveryResult
}

func newHTTPDelivery(env *domain.Envelope) *httpDelivery {
	return &httpDelivery{
		env:  env,
		done: make(chan deliveryResult, 1),
	}
}

func (d *httpDelivery) Envelope() *domain.Envelope { return d.env }

func (d *httpDelivery) Ack(_ context.Context) error {
	select {
	case d.done <- deliveryResult{}:
	default:
	}
	return nil
}

func (d *httpDelivery) Retry(_ context.Context, _ time.Duration, reason error) error {
	select {
	case d.done <- deliveryResult{err: reason}:
	default:
	}
	return nil
}

func (d *httpDelivery) Extend(_ context.Context, _ time.Time) error {
	return shared.ErrNotSupported
}
