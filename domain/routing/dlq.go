package routing

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// DLQEntry represents a dead-letter queue record.
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

// DLQFilter specifies criteria for querying DLQ entries.
type DLQFilter struct {
	RouteID  string
	Category string
	Since    time.Time
	Before   time.Time
	Limit    int
}
