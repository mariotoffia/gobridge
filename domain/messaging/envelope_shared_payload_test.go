package messaging

import (
	"runtime"
	"testing"
	"time"
)

func TestEnvelopeCloneSharesImmutablePayloadBacking(t *testing.T) {
	original := MustEnvelope(EnvelopeInput{
		ID:      "shared-payload",
		Payload: []byte("immutable"),
	})
	clone := original.Clone()

	if &original.payload[0] != &clone.payload[0] {
		t.Fatal("Clone allocated a second immutable payload backing")
	}

	clone.SetPayload([]byte("changed"))
	if got := string(original.Payload()); got != "immutable" {
		t.Fatalf("clone SetPayload mutated original: %q", got)
	}
	if got := string(clone.Payload()); got != "changed" {
		t.Fatalf("clone payload = %q, want changed", got)
	}

	exposed := original.Payload()
	exposed[0] = 'X'
	if got := string(original.Payload()); got != "immutable" {
		t.Fatalf("Payload exposed shared backing: %q", got)
	}
}

func TestNewEnvelopeFromImmutablePayloadSharesWithoutExposingBacking(t *testing.T) {
	payload := []byte("transport-owned")
	now := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	envelope, err := NewEnvelopeFromImmutablePayload(EnvelopeInput{
		ID:        "transport-payload",
		Payload:   payload,
		CreatedAt: now,
	}, now)
	if err != nil {
		t.Fatalf("NewEnvelopeFromImmutablePayload: %v", err)
	}
	if &payload[0] != &envelope.payload[0] {
		t.Fatal("trusted immutable construction copied payload backing")
	}
	exposed := envelope.Payload()
	exposed[0] = 'X'
	if got := string(envelope.Payload()); got != "transport-owned" {
		t.Fatalf("Payload exposed immutable transport backing: %q", got)
	}
}

func BenchmarkEnvelopeCloneSharedPayload(b *testing.B) {
	envelope := MustEnvelope(EnvelopeInput{
		ID:      "shared-payload-benchmark",
		Payload: make([]byte, 1<<20),
	})
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for range b.N {
		clone := envelope.Clone()
		runtime.KeepAlive(clone)
	}
}
