package route

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

func TestSharedOutboxFanoutPreservesImmutablePayloadOwnership(t *testing.T) {
	envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "shared-outbox-payload",
		Payload: []byte("immutable"),
	})
	runner := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "payload-owner",
		Bindings: []routing.DestinationBinding{
			{ID: "one", SessionID: "session-one", Address: "one"},
			{ID: "two", SessionID: "session-two", Address: "two"},
		},
	})
	records, err := runner.buildOutboxRecords(context.Background(), envelope, []routing.DispatchPlan{
		{BindingID: "one", Address: "one"},
		{BindingID: "two", Address: "two"},
	})
	if err != nil {
		t.Fatalf("buildOutboxRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}

	first := records[0].Snapshot()
	exposed := first.Payload()
	exposed[0] = 'X'
	first.SetPayload([]byte("changed"))

	if got := string(envelope.Payload()); got != "immutable" {
		t.Fatalf("source payload changed through fan-out record: %q", got)
	}
	if got := string(records[0].Snapshot().Payload()); got != "immutable" {
		t.Fatalf("first record payload changed through snapshot: %q", got)
	}
	if got := string(records[1].Snapshot().Payload()); got != "immutable" {
		t.Fatalf("sibling record payload changed through fan-out: %q", got)
	}
}

func TestMergeProcessedEnvelopeUsesCopyOnWritePayload(t *testing.T) {
	destination := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "destination",
		Payload: []byte("before"),
	})
	processed := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "processed",
		Payload: []byte("transformed"),
	})

	mergeProcessedEnvelope(destination, processed)
	processed.SetPayload([]byte("later"))

	if got := string(destination.Payload()); got != "transformed" {
		t.Fatalf("merged payload = %q, want transformed", got)
	}
}
