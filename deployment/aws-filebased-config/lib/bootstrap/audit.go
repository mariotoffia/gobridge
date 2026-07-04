package bootstrap

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

// slogAuditLogger implements ports.AuditLogger by writing structured audit
// events to the App's slog.Logger at Info level under the "audit" message.
//
// It deliberately mirrors httpapi.SlogAuditLogger (same attribute names, so
// runtime-side lease/DLQ audit and admin-API audit query identically in
// CloudWatch Logs Insights) without reusing that type: the architecture
// forbids injecting an httpapi component into the bridge layer, and audit
// emission is composition-root wiring, not HTTP-API behaviour.
type slogAuditLogger struct {
	logger *slog.Logger
}

var _ ports.AuditLogger = (*slogAuditLogger)(nil)

// newSlogAuditLogger creates a runtime audit logger writing to l.
func newSlogAuditLogger(l *slog.Logger) *slogAuditLogger {
	return &slogAuditLogger{logger: l}
}

func (a *slogAuditLogger) Log(_ context.Context, ev ports.AuditEvent) {
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
