package events

import "time"

// SchemaVersion is the semantic version of a concrete domain event
// payload. It is part of the on-the-wire contract: bumping the
// SchemaVersion of an existing EventType is a breaking change for
// downstream consumers and MUST be coordinated.
type SchemaVersion string

// Event is the contract every domain event satisfies. The five
// accessors are the minimum metadata required for routing,
// correlation, ordering, and schema evolution; concrete events embed
// Header to inherit the implementation and add typed payload fields.
//
// Implementations are immutable: constructors take all required state
// up-front and return the populated value. There are no setters.
type Event interface {
	// EventID returns the globally-unique identifier of this event
	// instance. Producers SHOULD use a v4 UUID or equivalent so the
	// identifier is collision-free across producers.
	EventID() string

	// EventType returns the canonical, namespaced event type string
	// (e.g. "persistence.outbox.claimed"). Consumers route on this
	// value; it is stable for the lifetime of a SchemaVersion.
	EventType() string

	// OccurredAt returns the wall-clock time at which the fact
	// occurred. Producers MUST use UTC.
	OccurredAt() time.Time

	// AggregateID returns the identity of the aggregate that
	// produced this event. Together with EventType it scopes the
	// event to a single producer instance.
	AggregateID() string

	// SchemaVersion returns the semantic version of the payload
	// schema. Producers bump this on breaking payload changes;
	// consumers MAY refuse unknown major versions.
	SchemaVersion() SchemaVersion
}

// Header is the common metadata embedded by every concrete event in
// this package. Fields are exported so events serialise cleanly to
// JSON without custom MarshalJSON implementations; the value-receiver
// accessors satisfy the Event interface.
type Header struct {
	ID        string        `json:"event_id"`
	Type      string        `json:"event_type"`
	Occurred  time.Time     `json:"occurred_at"`
	Aggregate string        `json:"aggregate_id"`
	Schema    SchemaVersion `json:"schema_version"`
}

// EventID implements Event.
func (h Header) EventID() string { return h.ID }

// EventType implements Event.
func (h Header) EventType() string { return h.Type }

// OccurredAt implements Event.
func (h Header) OccurredAt() time.Time { return h.Occurred }

// AggregateID implements Event.
func (h Header) AggregateID() string { return h.Aggregate }

// SchemaVersion implements Event.
func (h Header) SchemaVersion() SchemaVersion { return h.Schema }

// newHeader builds a Header for a concrete event constructor. Time is
// normalised to UTC; producers MUST supply a non-zero eventID and
// aggregateID -- empty strings are caller bugs and the constructor
// returns them unchanged for visibility in tests.
func newHeader(eventID, eventType, aggregateID string, occurredAt time.Time, schema SchemaVersion) Header {
	return Header{
		ID:        eventID,
		Type:      eventType,
		Occurred:  occurredAt.UTC(),
		Aggregate: aggregateID,
		Schema:    schema,
	}
}
