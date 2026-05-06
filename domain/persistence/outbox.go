package persistence

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// OutboxStatus represents the state of an outbox record in the state machine.
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxClaimed   OutboxStatus = "claimed"
	OutboxCompleted OutboxStatus = "completed"
	OutboxExpired   OutboxStatus = "expired"
)

// OutboxPartitionKey computes the outbox partition key from a record's
// session or binding identity. This is the canonical key used by
// OutboxStore.Persist, Claim, and QueryPending.
func OutboxPartitionKey(sessionID, bindingID string) string {
	if sessionID != "" {
		return "SESSION#" + sessionID
	}
	return "BINDING#" + bindingID
}

// OutboxRecord represents a durable outbox entry for reliable egress.
type OutboxRecord struct {
	ID              string
	RouteID         string
	EnvelopeID      string
	BindingID       string
	SessionID       string
	Address         string
	Envelope        messaging.Envelope
	DispatchHeaders map[string]any
	Status          OutboxStatus
	ClaimedBy       string
	ClaimedAt       time.Time
	ClaimVersion    uint64
	ReplayCount     int
	CreatedAt       time.Time
	ExpiresAt       time.Time
	CompletedAt     time.Time
}
