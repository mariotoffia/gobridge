package httpapi

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

// SlogAuditLogger implements ports.AuditLogger by writing structured
// audit events to a slog.Logger at Info level with an "audit" group.
type SlogAuditLogger struct {
	logger *slog.Logger
}

// NewSlogAuditLogger creates an audit logger that writes to the given
// slog.Logger.
func NewSlogAuditLogger(l *slog.Logger) *SlogAuditLogger {
	return &SlogAuditLogger{logger: l}
}

func (a *SlogAuditLogger) Log(_ context.Context, ev ports.AuditEvent) {
	attrs := []any{
		slog.String("action", ev.Action),
		slog.String("actor", ev.Actor),
		slog.String("resource", ev.Resource),
		slog.String("outcome", ev.Outcome),
		slog.Time("event_time", ev.Timestamp),
	}
	if ev.ResourceID != "" {
		attrs = append(attrs, slog.String("resource_id", ev.ResourceID))
	}
	for k, v := range ev.Detail {
		attrs = append(attrs, slog.Any(k, v))
	}
	a.logger.Info("audit", attrs...)
}
