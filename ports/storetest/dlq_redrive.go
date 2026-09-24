package storetest

import (
	"context"
	"maps"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// dlqRedriveFieldsRoundTrip pins that a store keeps a record's redrive mode and
// the facts a redrive trigger matches on (ADR 0019), and that a record without
// them reads back as manual with no facts.
func dlqRedriveFieldsRoundTrip(t *testing.T, store ports.DLQStore) {
	ctx := context.Background()
	facts := map[string]string{
		routing.ExtraInfoSessionID:       "plant-a",
		routing.ExtraInfoSubscription:    "sensors/+/temp",
		routing.ExtraInfoManagedIdentity: "broker-1/plant-a",
	}
	auto := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "rd-auto", RouteID: "route-rd", Category: "permanent", ErrorCode: "SUBSCRIPTION_REMOVED",
		FailedAt: dlqT1, RedriveMode: routing.RedriveAuto, ExtraInfo: facts,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-rd-auto", Subject: "sensors/1/temp"}),
	})
	manual := makeDLQEntry("rd-manual", "route-rd", "permanent", dlqT2)
	for _, e := range []routing.DLQEntry{auto, manual} {
		if err := store.Write(ctx, e); err != nil {
			t.Fatalf("write %s: %v", e.ID(), err)
		}
	}
	check := func(where string, got routing.DLQEntry) {
		t.Helper()
		switch got.ID() {
		case "rd-auto":
			if got.RedriveMode() != routing.RedriveAuto || !maps.Equal(got.ExtraInfo(), facts) {
				t.Fatalf("%s rd-auto: mode %q info %v, want auto %v", where, got.RedriveMode(), got.ExtraInfo(), facts)
			}
		case "rd-manual":
			if got.RedriveMode() != routing.RedriveManual || len(got.ExtraInfo()) != 0 {
				t.Fatalf("%s rd-manual: mode %q info %v, want manual and none", where, got.RedriveMode(), got.ExtraInfo())
			}
		}
	}
	for _, id := range []string{"rd-auto", "rd-manual"} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		check("Get", got)
	}
	listed, err := store.List(ctx, routing.DLQFilter{RouteID: "route-rd"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("list returned %d entries, want 2", len(listed))
	}
	for _, e := range listed {
		check("List", e)
	}
}
