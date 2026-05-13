package events

import "time"

// Canonical event types for the persistence/outbox aggregate.
const (
	TypeOutboxRecordClaimed   = "persistence.outbox.claimed"
	TypeOutboxRecordCompleted = "persistence.outbox.completed"
	TypeOutboxRecordExpired   = "persistence.outbox.expired"
)

// Schema versions for the outbox events. Bumping a version is a
// breaking change for downstream consumers and MUST be coordinated.
const (
	SchemaOutboxRecordClaimedV1   SchemaVersion = "1.0.0"
	SchemaOutboxRecordCompletedV1 SchemaVersion = "1.0.0"
	SchemaOutboxRecordExpiredV1   SchemaVersion = "1.0.0"
)

// OutboxRecordClaimed is emitted when an OutboxRecord transitions to
// the Claimed state under a fencing-token version. ClaimVersion is
// the monotonically increasing token recorded by the lease that
// authorised the claim; ReplayCount is the post-increment value
// (>= 1 on first claim).
type OutboxRecordClaimed struct {
	Header
	RouteID      string `json:"route_id"`
	BindingID    string `json:"binding_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	EnvelopeID   string `json:"envelope_id"`
	ClaimedBy    string `json:"claimed_by"`
	ClaimVersion uint64 `json:"claim_version"`
	ReplayCount  int    `json:"replay_count"`
}

// NewOutboxRecordClaimed constructs the event. Callers supply the
// event identity (eventID) and the wall-clock time of the transition;
// the aggregate identity is recordID.
func NewOutboxRecordClaimed(
	eventID, recordID string,
	occurredAt time.Time,
	routeID, bindingID, sessionID, envelopeID, claimedBy string,
	claimVersion uint64,
	replayCount int,
) OutboxRecordClaimed {
	return OutboxRecordClaimed{
		Header: newHeader(eventID, TypeOutboxRecordClaimed, recordID,
			occurredAt, SchemaOutboxRecordClaimedV1),
		RouteID:      routeID,
		BindingID:    bindingID,
		SessionID:    sessionID,
		EnvelopeID:   envelopeID,
		ClaimedBy:    claimedBy,
		ClaimVersion: claimVersion,
		ReplayCount:  replayCount,
	}
}

// OutboxRecordCompleted is emitted when an OutboxRecord transitions
// from Claimed to Completed, i.e. the egress send succeeded under the
// authorising fencing token.
type OutboxRecordCompleted struct {
	Header
	RouteID      string `json:"route_id"`
	BindingID    string `json:"binding_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	EnvelopeID   string `json:"envelope_id"`
	ClaimedBy    string `json:"claimed_by"`
	ClaimVersion uint64 `json:"claim_version"`
}

// NewOutboxRecordCompleted constructs the event.
func NewOutboxRecordCompleted(
	eventID, recordID string,
	occurredAt time.Time,
	routeID, bindingID, sessionID, envelopeID, claimedBy string,
	claimVersion uint64,
) OutboxRecordCompleted {
	return OutboxRecordCompleted{
		Header: newHeader(eventID, TypeOutboxRecordCompleted, recordID,
			occurredAt, SchemaOutboxRecordCompletedV1),
		RouteID:      routeID,
		BindingID:    bindingID,
		SessionID:    sessionID,
		EnvelopeID:   envelopeID,
		ClaimedBy:    claimedBy,
		ClaimVersion: claimVersion,
	}
}

// OutboxRecordExpired is emitted when an OutboxRecord transitions to
// the Expired terminal state (TTL exceeded or admin expiration).
type OutboxRecordExpired struct {
	Header
	RouteID    string `json:"route_id"`
	BindingID  string `json:"binding_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	EnvelopeID string `json:"envelope_id"`
	Reason     string `json:"reason,omitempty"`
}

// NewOutboxRecordExpired constructs the event.
func NewOutboxRecordExpired(
	eventID, recordID string,
	occurredAt time.Time,
	routeID, bindingID, sessionID, envelopeID, reason string,
) OutboxRecordExpired {
	return OutboxRecordExpired{
		Header: newHeader(eventID, TypeOutboxRecordExpired, recordID,
			occurredAt, SchemaOutboxRecordExpiredV1),
		RouteID:    routeID,
		BindingID:  bindingID,
		SessionID:  sessionID,
		EnvelopeID: envelopeID,
		Reason:     reason,
	}
}
