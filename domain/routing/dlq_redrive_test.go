package routing_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// Verifies an entry keeps its own copy of ExtraInfo: neither the caller's map
// nor the map a getter returns can change what the entry holds (ADR 0019).
func TestNewDLQEntryOwnsItsExtraInfo(t *testing.T) {
	info := map[string]string{routing.ExtraInfoSessionID: "plant-a"}
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:          "e1",
		Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-1", Subject: "s"}),
		RedriveMode: routing.RedriveAuto,
		ExtraInfo:   info,
	})
	info[routing.ExtraInfoSessionID] = "changed-by-caller"
	got := entry.ExtraInfo()
	if got[routing.ExtraInfoSessionID] != "plant-a" {
		t.Fatalf("entry shares the caller's map: session_id = %q", got[routing.ExtraInfoSessionID])
	}
	got[routing.ExtraInfoSessionID] = "changed-through-getter"
	if entry.ExtraInfo()[routing.ExtraInfoSessionID] != "plant-a" {
		t.Fatal("ExtraInfo() returned the entry's own map, not a copy")
	}
	if entry.RedriveMode() != routing.RedriveAuto {
		t.Fatalf("RedriveMode = %q, want auto", entry.RedriveMode())
	}
}

// Verifies an entry built without redrive fields is manual and carries no
// ExtraInfo.
func TestDLQEntryIsManualWithoutExtraInfoByDefault(t *testing.T) {
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:       "e2",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-2", Subject: "s"}),
	})
	if entry.RedriveMode() != routing.RedriveManual {
		t.Fatalf("RedriveMode = %q, want manual (empty)", entry.RedriveMode())
	}
	if entry.ExtraInfo() != nil {
		t.Fatalf("ExtraInfo = %v, want nil", entry.ExtraInfo())
	}
}

// Verifies an entry built with an empty ExtraInfo map reports none: a store
// that decodes an empty map must read back the same as one that stored nothing.
func TestDLQEntryWithEmptyExtraInfoReportsNone(t *testing.T) {
	entry := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:        "e3",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-3", Subject: "s"}),
		ExtraInfo: map[string]string{},
	})
	if entry.ExtraInfo() != nil {
		t.Fatalf("ExtraInfo = %v, want nil", entry.ExtraInfo())
	}
}
