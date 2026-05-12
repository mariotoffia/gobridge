package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mariotoffia/gobridge/domain/events"
	"github.com/mariotoffia/gobridge/ports"
)

// AuditEventPublisher is a primary-side adapter that consumes typed
// domain events and projects them onto the ports.AuditLogger
// boundary. It is the bridge between the new domain-event egress
// port (ports.EventPublisher) and the existing structured-audit log
// httpapi already exposes -- callers wire one EventPublisher and the
// audit trail picks up every typed fact for free.
//
// The action is the canonical Event.EventType (e.g.
// "persistence.outbox.claimed"); the resource is the type minus its
// trailing verb (e.g. "persistence.outbox"); the resource ID is the
// AggregateID. The full typed payload is reflected onto AuditEvent
// .Detail via a JSON round-trip so consumers see schema_version,
// event_id, and every typed payload field as structured slog
// attributes.
type AuditEventPublisher struct {
	audit ports.AuditLogger
}

// NewAuditEventPublisher returns a publisher that emits each domain
// event as a structured ports.AuditEvent on the supplied audit
// logger. A nil audit logger is replaced with ports.NoopAuditLogger
// so the publisher is always safe to call.
func NewAuditEventPublisher(a ports.AuditLogger) *AuditEventPublisher {
	if a == nil {
		a = ports.NoopAuditLogger{}
	}
	return &AuditEventPublisher{audit: a}
}

// Publish satisfies ports.EventPublisher. It MUST NOT panic on a nil
// or partially-populated event: a publisher that crashes the runtime
// is a worse failure mode than a missing audit line.
func (p *AuditEventPublisher) Publish(ctx context.Context, ev events.Event) {
	if ev == nil {
		return
	}

	detail := map[string]any{
		"event_id":       ev.EventID(),
		"schema_version": string(ev.SchemaVersion()),
	}

	// Project the typed payload onto the audit Detail so consumers
	// keep every field. JSON tags on the event structs already give
	// us snake_case keys identical to the wire format.
	if data, err := json.Marshal(ev); err == nil {
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil {
			for k, v := range raw {
				if _, taken := detail[k]; taken {
					continue
				}
				detail[k] = v
			}
		}
	}

	p.audit.Log(ctx, ports.AuditEvent{
		Timestamp:  ev.OccurredAt(),
		Action:     ev.EventType(),
		Actor:      "domain",
		Resource:   resourceFromEventType(ev.EventType()),
		ResourceID: ev.AggregateID(),
		Outcome:    "fact",
		Detail:     detail,
	})
}

// resourceFromEventType strips the trailing verb segment from a
// canonical event-type string. "persistence.outbox.claimed" becomes
// "persistence.outbox"; an unsegmented type is returned unchanged so
// an unexpected catalog entry still produces a meaningful audit row.
func resourceFromEventType(t string) string {
	if i := strings.LastIndex(t, "."); i > 0 {
		return t[:i]
	}
	return t
}

// Compile-time assertion: AuditEventPublisher satisfies the port.
var _ ports.EventPublisher = (*AuditEventPublisher)(nil)
