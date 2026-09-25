package dynamodbdlq

import (
	"context"
	"maps"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// Verifies the item Write sends carries the redrive mode and facts (ADR 0019)
// and that unmarshalEntry reads them back; a manual record with no facts
// carries neither attribute, so its item matches one an older release wrote.
func TestItemRoundTripsRedriveFields(t *testing.T) {
	facts := map[string]string{
		routing.ExtraInfoSessionID:       "plant-a",
		routing.ExtraInfoSubscription:    "sensors/+/temp",
		routing.ExtraInfoManagedIdentity: "broker-1/plant-a",
	}
	auto := routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: "auto-1", RouteID: "route-1", FailedAt: time.UnixMilli(1_700_000_000_000),
		RedriveMode: routing.RedriveAuto, ExtraInfo: facts,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-auto-1", Subject: "sensors/1/temp"}),
	})
	f := &fakeDLQClient{}
	s := newDLQStore(f)
	if err := s.Write(context.Background(), auto); err != nil {
		t.Fatalf("write auto: %v", err)
	}
	if err := s.Write(context.Background(), testEntry("manual-1")); err != nil {
		t.Fatalf("write manual: %v", err)
	}
	if len(f.putItems) != 2 {
		t.Fatalf("expected 2 PutItem, got %d", len(f.putItems))
	}

	got, err := unmarshalEntry(f.putItems[0].Item)
	if err != nil {
		t.Fatalf("unmarshal auto: %v", err)
	}
	if got.RedriveMode() != routing.RedriveAuto || !maps.Equal(got.ExtraInfo(), facts) {
		t.Fatalf("auto: mode %q info %v, want auto %v", got.RedriveMode(), got.ExtraInfo(), facts)
	}

	manualItem := f.putItems[1].Item
	for _, attr := range []string{attrRedriveMode, attrExtraInfo} {
		if _, ok := manualItem[attr]; ok {
			t.Fatalf("manual item carries %q; a manual record with no facts must omit it", attr)
		}
	}
	got, err = unmarshalEntry(manualItem)
	if err != nil {
		t.Fatalf("unmarshal manual: %v", err)
	}
	if got.RedriveMode() != routing.RedriveManual || got.ExtraInfo() != nil {
		t.Fatalf("manual: mode %q info %v, want manual and nil", got.RedriveMode(), got.ExtraInfo())
	}
}

// Verifies an item written before the redrive attributes existed reads back as
// a manual record with no facts.
func TestItemWithoutRedriveAttributesReadsAsManual(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{
		attrPK:      &ddbtypes.AttributeValueMemberS{Value: dlqKey("legacy-1")},
		attrRouteID: &ddbtypes.AttributeValueMemberS{Value: "route-1"},
	}
	got, err := unmarshalEntry(item)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID() != "legacy-1" || got.RedriveMode() != routing.RedriveManual || got.ExtraInfo() != nil {
		t.Fatalf("legacy item: id %q mode %q info %v, want legacy-1 manual nil", got.ID(), got.RedriveMode(), got.ExtraInfo())
	}
}
