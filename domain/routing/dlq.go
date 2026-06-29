package routing

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// DLQEntry represents a dead-letter queue record. The embedded envelope
// is private and isolated: NewDLQEntry deep-clones the supplied envelope
// so it cannot be mutated through references retained by the caller, and
// Snapshot returns a clone for any read or hand-off (the DDD R5
// aggregate-snapshot rule). All identity fields are private; access is
// via read-only value-receiver accessors. DLQEntry is passed by value
// through ports.DLQStore, so a copy's accessor values cannot affect
// another copy or the persisted entry.
//
// Storage adapters materializing an entry from a durable row MUST use
// RehydrateDLQEntry, which assigns the already-owned envelope without a
// redundant clone; runtime code MUST construct through NewDLQEntry.
//
// aggregate-root
type DLQEntry struct {
	id            string
	envelope      messaging.Envelope
	routeID       string
	bindingID     string
	address       string
	sessionID     string
	sourceID      string
	correlationID string
	reason        string
	category      string
	errorCode     string
	lastError     string
	failedAt      time.Time
	attempts      int
}

// ID returns the DLQ entry identifier.
func (e DLQEntry) ID() string { return e.id }

// RouteID returns the route that produced this entry.
func (e DLQEntry) RouteID() string { return e.routeID }

// BindingID returns the binding identifier associated with the entry.
func (e DLQEntry) BindingID() string { return e.bindingID }

// Address returns the transport destination address of the failed delivery.
func (e DLQEntry) Address() string { return e.address }

// SessionID returns the session identifier associated with the entry.
func (e DLQEntry) SessionID() string { return e.sessionID }

// SourceID returns the source identifier associated with the entry.
func (e DLQEntry) SourceID() string { return e.sourceID }

// CorrelationID returns the correlation identifier propagated from the envelope.
func (e DLQEntry) CorrelationID() string { return e.correlationID }

// Reason returns the human-readable failure reason.
func (e DLQEntry) Reason() string { return e.reason }

// Category returns the error classification category (e.g. "transient", "permanent").
func (e DLQEntry) Category() string { return e.category }

// ErrorCode returns the machine-readable error code.
func (e DLQEntry) ErrorCode() string { return e.errorCode }

// LastError returns the last error message recorded for this entry.
func (e DLQEntry) LastError() string { return e.lastError }

// FailedAt returns the time at which the delivery failure was recorded.
func (e DLQEntry) FailedAt() time.Time { return e.failedAt }

// Attempts returns the number of delivery attempts made before DLQ routing.
func (e DLQEntry) Attempts() int { return e.attempts }

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
	spec.Envelope = *spec.Envelope.Clone()
	return RehydrateDLQEntry(spec)
}

// RehydrateDLQEntry materializes a DLQEntry from a spec WITHOUT cloning
// the envelope. It is the storage-adapter boundary for entries read back
// from durable rows, where the envelope was freshly decoded (JSON
// unmarshal, attribute decode) and is therefore already owned, so a
// second deep clone would be wasted work. Runtime code that still holds
// a live reference to the envelope MUST use NewDLQEntry instead.
func RehydrateDLQEntry(spec DLQEntrySpec) DLQEntry {
	return DLQEntry{
		id:            spec.ID,
		envelope:      spec.Envelope,
		routeID:       spec.RouteID,
		bindingID:     spec.BindingID,
		address:       spec.Address,
		sessionID:     spec.SessionID,
		sourceID:      spec.SourceID,
		correlationID: spec.CorrelationID,
		reason:        spec.Reason,
		category:      spec.Category,
		errorCode:     spec.ErrorCode,
		lastError:     spec.LastError,
		failedAt:      spec.FailedAt,
		attempts:      spec.Attempts,
	}
}

// Snapshot returns a deep copy of the entry's embedded envelope so
// callers can read, mutate, or hand off the envelope without
// compromising the entry's persisted state. It is the only read path to
// the envelope now that the field is unexported.
func (e DLQEntry) Snapshot() *messaging.Envelope {
	return e.envelope.Clone()
}

// DLQFilter specifies criteria for querying DLQ entries.
type DLQFilter struct {
	RouteID  string
	Category string
	Since    time.Time
	Before   time.Time
	Limit    int
}
