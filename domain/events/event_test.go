package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/events"
)

// fixedTime returns a deterministic, non-UTC timestamp so we can
// observe the constructor's UTC normalisation.
func fixedTime() time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(2026, 5, 12, 8, 30, 0, 0, loc)
}

// asEvent compile-checks that a concrete type satisfies the Event
// interface. Used as the body of assertions in the table-driven test
// below.
func asEvent(e events.Event) events.Event { return e }

// TestConcreteEvents_ImplementInterface_AndExposeMetadata asserts the
// full first-wave catalog satisfies events.Event and propagates the
// supplied identity, type, schema version, and UTC-normalised time.
func TestConcreteEvents_ImplementInterface_AndExposeMetadata(t *testing.T) {
	now := fixedTime()
	wantUTC := now.UTC()

	cases := []struct {
		name       string
		ev         events.Event
		wantType   string
		wantSchema events.SchemaVersion
		wantAggID  string
	}{
		{
			name:       "OutboxRecordClaimed",
			ev:         events.NewOutboxRecordClaimed("e-1", "rec-1", now, "route-A", "bind-1", "sess-1", "env-1", "owner-X", 7, 1),
			wantType:   events.TypeOutboxRecordClaimed,
			wantSchema: events.SchemaOutboxRecordClaimedV1,
			wantAggID:  "rec-1",
		},
		{
			name:       "OutboxRecordCompleted",
			ev:         events.NewOutboxRecordCompleted("e-2", "rec-1", now, "route-A", "bind-1", "sess-1", "env-1", "owner-X", 7),
			wantType:   events.TypeOutboxRecordCompleted,
			wantSchema: events.SchemaOutboxRecordCompletedV1,
			wantAggID:  "rec-1",
		},
		{
			name:       "OutboxRecordExpired",
			ev:         events.NewOutboxRecordExpired("e-3", "rec-1", now, "route-A", "bind-1", "sess-1", "env-1", "ttl_exceeded"),
			wantType:   events.TypeOutboxRecordExpired,
			wantSchema: events.SchemaOutboxRecordExpiredV1,
			wantAggID:  "rec-1",
		},
		{
			name:       "LeaseAcquired",
			ev:         events.NewLeaseAcquired("e-4", "lease-1", now, "owner-X", 11, now.Add(30*time.Second)),
			wantType:   events.TypeLeaseAcquired,
			wantSchema: events.SchemaLeaseAcquiredV1,
			wantAggID:  "lease-1",
		},
		{
			name:       "LeaseRenewed",
			ev:         events.NewLeaseRenewed("e-5", "lease-1", now, "owner-X", 11, now.Add(60*time.Second)),
			wantType:   events.TypeLeaseRenewed,
			wantSchema: events.SchemaLeaseRenewedV1,
			wantAggID:  "lease-1",
		},
		{
			name:       "LeaseLost",
			ev:         events.NewLeaseLost("e-6", "lease-1", now, "owner-X", 11, "preempted"),
			wantType:   events.TypeLeaseLost,
			wantSchema: events.SchemaLeaseLostV1,
			wantAggID:  "lease-1",
		},
		{
			name:       "DLQEntryRecorded",
			ev:         events.NewDLQEntryRecorded("e-7", "dlq-1", now, "route-A", "bind-1", "sess-1", "env-1", "permanent", "INVALID_PAYLOAD", "schema mismatch", 4),
			wantType:   events.TypeDLQEntryRecorded,
			wantSchema: events.SchemaDLQEntryRecordedV1,
			wantAggID:  "dlq-1",
		},
		{
			name:       "DLQEntryRedriven",
			ev:         events.NewDLQEntryRedriven("e-8", "dlq-1", now, "route-A", "env-1", "operator@example"),
			wantType:   events.TypeDLQEntryRedriven,
			wantSchema: events.SchemaDLQEntryRedrivenV1,
			wantAggID:  "dlq-1",
		},
		{
			name:       "BlueprintCommitted",
			ev:         events.NewBlueprintCommitted("e-9", "blueprint-default", now, "rev-42", "sha256:abcd", "operator@example"),
			wantType:   events.TypeBlueprintCommitted,
			wantSchema: events.SchemaBlueprintCommittedV1,
			wantAggID:  "blueprint-default",
		},
		{
			name:       "CredentialRotated",
			ev:         events.NewCredentialRotated("e-10", "cred://aws/ssm/key", now, "cred://aws/ssm/key", "ssm", "v1", "v2", "rotator"),
			wantType:   events.TypeCredentialRotated,
			wantSchema: events.SchemaCredentialRotatedV1,
			wantAggID:  "cred://aws/ssm/key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := asEvent(tc.ev)
			if ev.EventType() != tc.wantType {
				t.Fatalf("EventType: got %q, want %q", ev.EventType(), tc.wantType)
			}
			if ev.SchemaVersion() != tc.wantSchema {
				t.Fatalf("SchemaVersion: got %q, want %q", ev.SchemaVersion(), tc.wantSchema)
			}
			if ev.AggregateID() != tc.wantAggID {
				t.Fatalf("AggregateID: got %q, want %q", ev.AggregateID(), tc.wantAggID)
			}
			if ev.EventID() == "" {
				t.Fatalf("EventID: empty")
			}
			if !ev.OccurredAt().Equal(wantUTC) {
				t.Fatalf("OccurredAt: got %v, want %v (UTC of %v)", ev.OccurredAt(), wantUTC, now)
			}
			if ev.OccurredAt().Location() != time.UTC {
				t.Fatalf("OccurredAt.Location: got %v, want UTC", ev.OccurredAt().Location())
			}
		})
	}
}

// TestEvent_JSONRoundTrip verifies the wire format carries every
// header field (event_id, event_type, occurred_at, aggregate_id,
// schema_version) plus typed payload, and that re-marshalling a
// decoded payload yields the same bytes (deterministic encoding).
func TestEvent_JSONRoundTrip(t *testing.T) {
	now := fixedTime()
	original := events.NewOutboxRecordClaimed("e-rt", "rec-rt", now, "route-A", "bind-1", "sess-1", "env-1", "owner-X", 9, 2)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Header fields must all be present on the wire.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, field := range []string{"event_id", "event_type", "occurred_at", "aggregate_id", "schema_version"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("missing wire field %q in %s", field, string(data))
		}
	}
	if got := raw["schema_version"].(string); events.SchemaVersion(got) != events.SchemaOutboxRecordClaimedV1 {
		t.Fatalf("schema_version: got %q, want %q", got, events.SchemaOutboxRecordClaimedV1)
	}

	// Typed round-trip preserves payload semantics.
	var decoded events.OutboxRecordClaimed
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal typed: %v", err)
	}
	if decoded.EventID() != original.EventID() {
		t.Fatalf("EventID round-trip: got %q, want %q", decoded.EventID(), original.EventID())
	}
	if decoded.ClaimVersion != original.ClaimVersion || decoded.ReplayCount != original.ReplayCount {
		t.Fatalf("payload round-trip mismatch: got %+v, want %+v", decoded, original)
	}
	if !decoded.OccurredAt().Equal(original.OccurredAt()) {
		t.Fatalf("OccurredAt round-trip: got %v, want %v", decoded.OccurredAt(), original.OccurredAt())
	}

	// Re-encode and compare bytes for stability.
	data2, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(data) != string(data2) {
		t.Fatalf("encoding not stable:\n  first:  %s\n  second: %s", data, data2)
	}
}

// TestEventTypes_AreUnique guards against accidental collisions in
// the canonical event-type catalog. A duplicate would silently
// route two distinct facts into the same downstream handler.
func TestEventTypes_AreUnique(t *testing.T) {
	all := []string{
		events.TypeOutboxRecordClaimed,
		events.TypeOutboxRecordCompleted,
		events.TypeOutboxRecordExpired,
		events.TypeLeaseAcquired,
		events.TypeLeaseRenewed,
		events.TypeLeaseLost,
		events.TypeDLQEntryRecorded,
		events.TypeDLQEntryRedriven,
		events.TypeBlueprintCommitted,
		events.TypeCredentialRotated,
	}
	seen := map[string]bool{}
	for _, t0 := range all {
		if t0 == "" {
			t.Fatalf("empty event type in catalog")
		}
		if seen[t0] {
			t.Fatalf("duplicate event type: %q", t0)
		}
		seen[t0] = true
	}
}
