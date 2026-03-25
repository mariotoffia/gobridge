package ports

import (
	"context"
	"time"
)

// AuditEvent represents a structured audit log entry for security-relevant
// operations such as admin API calls, DLQ mutations, and lease transitions.
type AuditEvent struct {
	Timestamp  time.Time
	Action     string
	Actor      string
	Resource   string
	ResourceID string
	Outcome    string
	Detail     map[string]any
}

// AuditLogger emits structured audit events. Implementations must be
// safe for concurrent use.
type AuditLogger interface {
	Log(ctx context.Context, event AuditEvent)
}

// NoopAuditLogger discards all audit events.
type NoopAuditLogger struct{}

func (NoopAuditLogger) Log(context.Context, AuditEvent) {}
