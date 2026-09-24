package dlq_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// Verifies the AutoRedrive option writes an auto entry carrying the facts as
// passed, and that the caller changing its map after Route cannot reach the
// written entry (ADR 0019).
func TestRouteAutoRedriveMarksTheEntry(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-1", Subject: "sensors/1/temp"})
	info := map[string]string{
		routing.ExtraInfoSessionID:       "plant-a",
		routing.ExtraInfoSubscription:    "sensors/+/temp",
		routing.ExtraInfoManagedIdentity: "id-1",
	}
	if err := r.Route(t.Context(), env, "r1", "", "sensors/+/temp", "plant-a", "plant-a",
		shared.ErrSubscriptionRemoved, 0, dlq.AutoRedrive(info)); err != nil {
		t.Fatalf("Route: %v", err)
	}
	info[routing.ExtraInfoSubscription] = "changed-after-route"
	got := onlyWritten(t, store)
	if got.RedriveMode() != routing.RedriveAuto {
		t.Fatalf("RedriveMode = %q, want auto", got.RedriveMode())
	}
	if got.ExtraInfo()[routing.ExtraInfoSubscription] != "sensors/+/temp" {
		t.Fatalf("ExtraInfo = %v, want the facts as passed", got.ExtraInfo())
	}
}

// Verifies Route without options writes a manual entry with no ExtraInfo.
func TestRouteWithoutOptionsWritesAManualEntry(t *testing.T) {
	store := NewFakeStore()
	r := dlq.New(store)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-2", Subject: "a"})
	if err := r.Route(t.Context(), env, "r1", "", "a", "", "", shared.ErrMessageExpired, 0); err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := onlyWritten(t, store)
	if got.RedriveMode() != routing.RedriveManual || got.ExtraInfo() != nil {
		t.Fatalf("mode %q info %v, want manual and none", got.RedriveMode(), got.ExtraInfo())
	}
}

// onlyWritten fails the test unless the store holds exactly one entry, and
// returns it.
func onlyWritten(t *testing.T, store *FakeStore) routing.DLQEntry {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.Entries) != 1 {
		t.Fatalf("store holds %d entries, want exactly 1", len(store.Entries))
	}
	return store.Entries[0]
}
