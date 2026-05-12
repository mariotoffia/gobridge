package events

import "time"

// Canonical event types for the routing/DLQ aggregate.
const (
	TypeDLQEntryRecorded = "routing.dlq.recorded"
	TypeDLQEntryRedriven = "routing.dlq.redriven"
)

// Schema versions for DLQ events.
const (
	SchemaDLQEntryRecordedV1 SchemaVersion = "1.0.0"
	SchemaDLQEntryRedrivenV1 SchemaVersion = "1.0.0"
)

// DLQEntryRecorded is emitted when a message is durably written to
// the dead-letter queue. The event carries the routing context and
// classification (Category, ErrorCode, Reason) so consumers can
// filter without re-fetching the entry payload.
type DLQEntryRecorded struct {
	Header
	RouteID    string `json:"route_id"`
	BindingID  string `json:"binding_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	EnvelopeID string `json:"envelope_id"`
	Category   string `json:"category,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Attempts   int    `json:"attempts"`
}

// NewDLQEntryRecorded constructs the event.
func NewDLQEntryRecorded(
	eventID, entryID string,
	occurredAt time.Time,
	routeID, bindingID, sessionID, envelopeID string,
	category, errorCode, reason string,
	attempts int,
) DLQEntryRecorded {
	return DLQEntryRecorded{
		Header: newHeader(eventID, TypeDLQEntryRecorded, entryID,
			occurredAt, SchemaDLQEntryRecordedV1),
		RouteID:    routeID,
		BindingID:  bindingID,
		SessionID:  sessionID,
		EnvelopeID: envelopeID,
		Category:   category,
		ErrorCode:  errorCode,
		Reason:     reason,
		Attempts:   attempts,
	}
}

// DLQEntryRedriven is emitted when a DLQ entry is successfully
// re-injected onto its source route. RedrivenBy identifies the actor
// that initiated the redrive (e.g. an admin operator address).
type DLQEntryRedriven struct {
	Header
	RouteID    string `json:"route_id"`
	EnvelopeID string `json:"envelope_id"`
	RedrivenBy string `json:"redriven_by,omitempty"`
}

// NewDLQEntryRedriven constructs the event.
func NewDLQEntryRedriven(
	eventID, entryID string,
	occurredAt time.Time,
	routeID, envelopeID, redrivenBy string,
) DLQEntryRedriven {
	return DLQEntryRedriven{
		Header: newHeader(eventID, TypeDLQEntryRedriven, entryID,
			occurredAt, SchemaDLQEntryRedrivenV1),
		RouteID:    routeID,
		EnvelopeID: envelopeID,
		RedrivenBy: redrivenBy,
	}
}
