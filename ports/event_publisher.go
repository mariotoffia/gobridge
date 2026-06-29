package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/events"
)

// EventPublisher is the egress port for domain events. Events are
// best-effort observability FACTS: application and runtime services
// construct them via the public constructors in domain/events (for
// example events.NewOutboxRecordClaimed) and hand them to an
// EventPublisher supplied at composition time. They are NOT raised by
// domain aggregate transitions -- OutboxRecord.Claim/Complete/Expire
// and the other aggregate methods neither record nor return events;
// the producing service decides when a transition is worth publishing
// as a fact. Adapters fan the events out to whatever sink the
// deployment requires (audit log, message bus, durable outbox, …).
//
// Publish is intentionally NON-BLOCKING and BEST-EFFORT. It returns no
// error and MUST NOT block on a slow sink: under backpressure an
// implementation MAY drop the event rather than stall the caller's hot
// path. Because loss is therefore invisible to the caller, any
// implementation that buffers and can discard events MUST emit a drop
// counter metric on every discard so operators can observe event loss.
// NoopEventPublisher is the deliberate exception: it is the null sink
// and discards every event silently by definition.
//
// Implementations MUST be safe for concurrent use. A nil EventPublisher
// MUST be substituted with NoopEventPublisher{} at the composition root
// -- the runtime never branches on a nil port.
type EventPublisher interface {
	// Publish emits event as a best-effort fact. It never blocks the
	// caller and never returns an error; an implementation that drops
	// under backpressure MUST count the drop via a metric.
	Publish(ctx context.Context, event events.Event)
}

// NoopEventPublisher discards all events silently. It is the safe
// default when a deployment has no event sink configured, and is the
// documented exception to the drop-metric rule: as the null sink it has
// no backpressure and no metric to emit.
type NoopEventPublisher struct{}

// Publish satisfies EventPublisher by discarding the event. The drop is
// intentional and unmetered -- see NoopEventPublisher.
func (NoopEventPublisher) Publish(context.Context, events.Event) {}

// Compile-time assertion: NoopEventPublisher satisfies the port.
var _ EventPublisher = NoopEventPublisher{}
