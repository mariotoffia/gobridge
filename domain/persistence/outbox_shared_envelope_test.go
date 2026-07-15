package persistence

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

func TestNewOutboxRecordsSharesPrivateEnvelopeAndIsolatesSnapshots(t *testing.T) {
	now := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "fanout-envelope",
		Payload: []byte("immutable"),
		Headers: map[string]any{"application": "value"},
	})
	specs := []OutboxSpec{
		{
			ID: "record-one", EnvelopeID: envelope.ID(), BindingID: "one",
			SessionID: "session-one", Envelope: *envelope, CreatedAt: now,
		},
		{
			ID: "record-two", EnvelopeID: envelope.ID(), BindingID: "two",
			SessionID: "session-two", Envelope: *envelope, CreatedAt: now,
		},
	}

	records, err := NewOutboxRecords(*envelope, specs)
	if err != nil {
		t.Fatalf("NewOutboxRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}

	// Aggregate internals share one finalized private snapshot.
	records[0].envelope.SetHeader("private-test", "shared")
	if got, _ := records[1].envelope.Header("private-test"); got != "shared" {
		t.Fatalf("fan-out records did not share private envelope backing: %v", got)
	}

	// Public snapshots remain mutation-isolated despite the private sharing.
	snapshot := records[0].Snapshot()
	snapshot.SetHeader("private-test", "changed")
	snapshot.SetPayload([]byte("changed"))
	if got, _ := records[1].Snapshot().Header("private-test"); got != "shared" {
		t.Fatalf("snapshot mutated sibling record: %v", got)
	}
	if got := string(records[1].Snapshot().Payload()); got != "immutable" {
		t.Fatalf("snapshot mutated sibling payload: %q", got)
	}
}
