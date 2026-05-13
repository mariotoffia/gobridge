package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/events"
)

// EventPublisher is the egress port for domain events. Aggregates
// in the domain layer raise events.Event values; application
// services pass them to an EventPublisher implementation supplied at
// composition time. Adapters fan the events out to whatever sink the
// deployment requires (audit log, message bus, durable outbox, …).
//
// Implementations MUST be safe for concurrent use. Publish MUST NOT
// block on slow sinks: an overrun policy (drop, buffer, fail-fast)
// is an implementation concern. A nil EventPublisher MUST be
// substituted with NoopEventPublisher{} at the composition root --
// the runtime never branches on a nil port.
type EventPublisher interface {
	Publish(ctx context.Context, event events.Event)
}

// NoopEventPublisher discards all events. It is the safe default
// when a deployment has no event sink configured.
type NoopEventPublisher struct{}

// Publish satisfies EventPublisher by discarding the event.
func (NoopEventPublisher) Publish(context.Context, events.Event) {}

// Compile-time assertion: NoopEventPublisher satisfies the port.
var _ EventPublisher = NoopEventPublisher{}
