package routing

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// DLQEntry represents a dead-letter queue record. Use NewDLQEntry to
// construct entries: the constructor deep-clones the supplied envelope
// so the entry's Envelope cannot be mutated through references retained
// by the caller (DDD R5 aggregate-snapshot rule).
//
// Storage adapters that materialize entries from durable rows MAY
// populate the struct fields directly; runtime code MUST go through the
// constructor and access the envelope via Snapshot when the value will
// be handed to mutator code (e.g. the redrive path).
type DLQEntry struct {
	ID        string
	Envelope  messaging.Envelope
	RouteID   string
	BindingID string
	// Address is the transport destination address that was the
	// target of the failed delivery (e.g. MQTT topic, SQS queue URL,
	// AMQP routing key) on egress, or the source address on ingress.
	// It is the concrete transport-level address and is NOT the
	// logical Envelope.Subject. Empty when not known at the call
	// site.
	Address       string
	SessionID     string
	SourceID      string
	CorrelationID string
	Reason        string
	Category      string
	ErrorCode     string
	LastError     string
	FailedAt      time.Time
	Attempts      int
}

// DLQEntrySpec carries the inputs required to construct a DLQEntry.
// The supplied Envelope is deep-cloned by NewDLQEntry so callers may
// continue to use their reference without affecting the persisted
// entry.
type DLQEntrySpec struct {
	ID            string
	Envelope      messaging.Envelope
	RouteID       string
	BindingID     string
	Address       string
	SessionID     string
	SourceID      string
	CorrelationID string
	Reason        string
	Category      string
	ErrorCode     string
	LastError     string
	FailedAt      time.Time
	Attempts      int
}

// NewDLQEntry constructs a DLQEntry, deep-cloning the supplied envelope
// so the resulting entry owns an isolated copy. This is the snapshot
// boundary the DDD aggregate rules require: subsequent mutations of
// the input envelope (headers, payload) do not leak into the entry.
func NewDLQEntry(spec DLQEntrySpec) DLQEntry {
	return DLQEntry{
		ID:            spec.ID,
		Envelope:      *spec.Envelope.Clone(),
		RouteID:       spec.RouteID,
		BindingID:     spec.BindingID,
		Address:       spec.Address,
		SessionID:     spec.SessionID,
		SourceID:      spec.SourceID,
		CorrelationID: spec.CorrelationID,
		Reason:        spec.Reason,
		Category:      spec.Category,
		ErrorCode:     spec.ErrorCode,
		LastError:     spec.LastError,
		FailedAt:      spec.FailedAt,
		Attempts:      spec.Attempts,
	}
}

// Snapshot returns a deep copy of the entry's embedded envelope so
// callers can mutate or hand off the envelope without compromising the
// entry's persisted state.
func (e DLQEntry) Snapshot() *messaging.Envelope {
	return e.Envelope.Clone()
}

// DLQFilter specifies criteria for querying DLQ entries.
type DLQFilter struct {
	RouteID  string
	Category string
	Since    time.Time
	Before   time.Time
	Limit    int
}
