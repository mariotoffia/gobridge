package httpapi_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/events"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

type recordingAudit struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (r *recordingAudit) Log(_ context.Context, ev ports.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingAudit) last() ports.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return ports.AuditEvent{}
	}
	return r.events[len(r.events)-1]
}

// TestAuditEventPublisher_PublishesTypedFacts asserts the publisher
// projects a domain event onto an AuditEvent that carries the
// canonical event type as Action, the schema version in Detail, and
// every typed payload field reflected through the JSON round-trip.
func TestAuditEventPublisher_PublishesTypedFacts(t *testing.T) {
	a := &recordingAudit{}
	p := httpapi.NewAuditEventPublisher(a)

	now := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	ev := events.NewOutboxRecordClaimed(
		"e-1", "rec-7", now,
		"route-A", "bind-1", "sess-1", "env-1",
		"owner-X", 11, 3,
	)

	p.Publish(context.Background(), ev)

	got := a.last()
	if got.Action != events.TypeOutboxRecordClaimed {
		t.Fatalf("Action: got %q, want %q", got.Action, events.TypeOutboxRecordClaimed)
	}
	if got.Resource != "persistence.outbox" {
		t.Fatalf("Resource: got %q, want %q", got.Resource, "persistence.outbox")
	}
	if got.ResourceID != "rec-7" {
		t.Fatalf("ResourceID: got %q, want %q", got.ResourceID, "rec-7")
	}
	if got.Outcome != "fact" {
		t.Fatalf("Outcome: got %q, want %q", got.Outcome, "fact")
	}
	if !got.Timestamp.Equal(now) {
		t.Fatalf("Timestamp: got %v, want %v", got.Timestamp, now)
	}

	if got.Detail["schema_version"] != string(events.SchemaOutboxRecordClaimedV1) {
		t.Fatalf("Detail.schema_version: got %v, want %v",
			got.Detail["schema_version"], events.SchemaOutboxRecordClaimedV1)
	}
	if got.Detail["event_id"] != "e-1" {
		t.Fatalf("Detail.event_id: got %v, want e-1", got.Detail["event_id"])
	}
	// Typed payload reflected through.
	if got.Detail["route_id"] != "route-A" {
		t.Fatalf("Detail.route_id: got %v, want route-A", got.Detail["route_id"])
	}
	if got.Detail["claimed_by"] != "owner-X" {
		t.Fatalf("Detail.claimed_by: got %v, want owner-X", got.Detail["claimed_by"])
	}
	// JSON numbers decode to float64.
	if v, ok := got.Detail["claim_version"].(float64); !ok || v != 11 {
		t.Fatalf("Detail.claim_version: got %#v, want 11", got.Detail["claim_version"])
	}
}

// TestAuditEventPublisher_NilEventIsSafe ensures the publisher does
// not panic on a nil event -- a misuse should produce no audit row,
// not crash the runtime.
func TestAuditEventPublisher_NilEventIsSafe(t *testing.T) {
	a := &recordingAudit{}
	p := httpapi.NewAuditEventPublisher(a)

	p.Publish(context.Background(), nil)

	if len(a.events) != 0 {
		t.Fatalf("expected no audit events for nil input, got %d", len(a.events))
	}
}

// TestAuditEventPublisher_NilLoggerSubstitutesNoop verifies the
// constructor's nil-safety contract: a nil ports.AuditLogger MUST be
// replaced with the noop logger so callers never branch on nil.
func TestAuditEventPublisher_NilLoggerSubstitutesNoop(t *testing.T) {
	p := httpapi.NewAuditEventPublisher(nil)
	now := time.Now().UTC()
	ev := events.NewLeaseAcquired("e-x", "lease-1", now, "owner", 1, now.Add(time.Second))

	// Must not panic.
	p.Publish(context.Background(), ev)
}

// TestAuditEventPublisher_SatisfiesPort is a structural compile-time
// guard expressed as a runtime assertion so a future signature drift
// in ports.EventPublisher fails this test, not just lint.
func TestAuditEventPublisher_SatisfiesPort(t *testing.T) {
	var _ ports.EventPublisher = httpapi.NewAuditEventPublisher(ports.NoopAuditLogger{})
}
