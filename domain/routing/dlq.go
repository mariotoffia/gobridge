package routing

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// DLQEntry represents a dead-letter queue record.
type DLQEntry struct {
	ID            string
	Envelope      messaging.Envelope
	RouteID       string
	BindingID     string
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

// DLQFilter specifies criteria for querying DLQ entries.
type DLQFilter struct {
	RouteID  string
	Category string
	Since    time.Time
	Before   time.Time
	Limit    int
}
