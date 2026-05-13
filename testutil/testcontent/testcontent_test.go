package testcontent

import (
	"fmt"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// mockT captures Errorf/Fatalf calls for verification.
type mockT struct {
	mu     sync.Mutex
	errors []string
	fatals []string
}

func (m *mockT) Helper() {}

func (m *mockT) Errorf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, fmt.Sprintf(format, args...))
}

func (m *mockT) Fatalf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fatals = append(m.fatals, fmt.Sprintf(format, args...))
}

func (m *mockT) errCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.errors)
}

// -----------------------------------------------------------------------
// Tag / ExtractTID
// -----------------------------------------------------------------------

func TestTag_SetsHeaderAndPayloadTID(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test-topic",
		Payload: []byte(`{"key":"value"}`),
	})

	tid, exp := Tag(env)
	if tid == "" {
		t.Fatal("Tag returned empty TID")
	}
	if exp.TID != tid {
		t.Fatalf("Expected.TID=%q, want %q", exp.TID, tid)
	}

	got := ExtractTID(env)
	if got != tid {
		t.Fatalf("ExtractTID from header: got %q, want %q", got, tid)
	}

	payloadTID := extractPayloadTID(env.Payload)
	if payloadTID != tid {
		t.Fatalf("payload _tid=%q, want %q", payloadTID, tid)
	}
}

func TestTag_EmptyPayload(t *testing.T) {
	env := &messaging.Envelope{Payload: nil}
	tid, _ := Tag(env)
	if tid == "" {
		t.Fatal("Tag returned empty TID for nil payload")
	}
	got := ExtractTID(env)
	if got != tid {
		t.Fatalf("ExtractTID from header: got %q, want %q (nil payload)", got, tid)
	}
}

func TestTag_NonJSONPayload(t *testing.T) {
	env := &messaging.Envelope{Payload: []byte("plain text")}
	tid, _ := Tag(env)
	got := ExtractTID(env)
	if got != tid {
		t.Fatalf("ExtractTID: got %q, want %q (non-JSON)", got, tid)
	}
	// Payload should be unchanged since it's not a JSON object.
	if string(env.Payload) != "plain text" {
		t.Fatalf("payload changed: %q", env.Payload)
	}
}

func TestExtractTID_HeaderFallback(t *testing.T) {
	env := &messaging.Envelope{
		Payload: []byte(`{"_tid":"from-payload","key":"value"}`),
	}
	got := ExtractTID(env)
	if got != "from-payload" {
		t.Fatalf("ExtractTID payload fallback: got %q, want %q", got, "from-payload")
	}
}

func TestExtractTID_Empty(t *testing.T) {
	env := &messaging.Envelope{Payload: []byte("not json")}
	got := ExtractTID(env)
	if got != "" {
		t.Fatalf("ExtractTID should be empty for no-header+no-json, got %q", got)
	}
}

func TestTagN(t *testing.T) {
	envs := make([]*messaging.Envelope, 5)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: fmt.Sprintf("topic-%d", i),
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
		})
	}
	exps := TagN(envs)
	if len(exps) != 5 {
		t.Fatalf("TagN returned %d, want 5", len(exps))
	}
	seen := make(map[string]bool)
	for _, e := range exps {
		if seen[e.TID] {
			t.Fatalf("duplicate TID %q", e.TID)
		}
		seen[e.TID] = true
	}
}

// -----------------------------------------------------------------------
// ReceivedFromEnvelopes / ReceivedFromBodies
// -----------------------------------------------------------------------

func TestReceivedFromEnvelopes(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Payload: []byte(`{"key":"val"}`),
		Subject: "s",
	})
	tid, _ := Tag(env)
	rx := ReceivedFromEnvelopes([]*messaging.Envelope{env})
	if len(rx) != 1 {
		t.Fatalf("len=%d, want 1", len(rx))
	}
	if rx[0].TID != tid {
		t.Fatalf("TID=%q, want %q", rx[0].TID, tid)
	}
}

func TestReceivedFromBodies(t *testing.T) {
	body := `{"_tid":"abc","key":"val"}`
	rx := ReceivedFromBodies([]string{body})
	if len(rx) != 1 {
		t.Fatalf("len=%d, want 1", len(rx))
	}
	if rx[0].TID != "abc" {
		t.Fatalf("TID=%q, want %q", rx[0].TID, "abc")
	}
}

// -----------------------------------------------------------------------
// AssertReceivedSet
// -----------------------------------------------------------------------

func TestAssertReceivedSet_Match(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a"}, {TID: "b"}}
	rx := []Received{{TID: "b"}, {TID: "a"}}
	AssertReceivedSet(mt, sent, rx)
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertReceivedSet_Missing(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a"}, {TID: "b"}, {TID: "c"}}
	rx := []Received{{TID: "a"}}
	AssertReceivedSet(mt, sent, rx)
	if mt.errCount() == 0 {
		t.Fatal("expected error for missing TIDs")
	}
}

func TestAssertReceivedSet_Extra(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a"}}
	rx := []Received{{TID: "a"}, {TID: "z"}}
	AssertReceivedSet(mt, sent, rx)
	if mt.errCount() == 0 {
		t.Fatal("expected error for extra TIDs")
	}
}

// -----------------------------------------------------------------------
// AssertNoDuplicates
// -----------------------------------------------------------------------

func TestAssertNoDuplicates_OK(t *testing.T) {
	mt := &mockT{}
	rx := []Received{{TID: "a"}, {TID: "b"}, {TID: "c"}}
	AssertNoDuplicates(mt, rx)
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertNoDuplicates_Dups(t *testing.T) {
	mt := &mockT{}
	rx := []Received{{TID: "a"}, {TID: "b"}, {TID: "a"}}
	AssertNoDuplicates(mt, rx)
	if mt.errCount() == 0 {
		t.Fatal("expected error for duplicates")
	}
}

// -----------------------------------------------------------------------
// AssertOrdered
// -----------------------------------------------------------------------

func TestAssertOrdered_OK(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "1"}, {TID: "2"}, {TID: "3"}}
	rx := []Received{{TID: "1"}, {TID: "2"}, {TID: "3"}}
	AssertOrdered(mt, sent, rx)
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertOrdered_WithGaps(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "1"}, {TID: "2"}, {TID: "3"}}
	rx := []Received{{TID: "x"}, {TID: "1"}, {TID: "y"}, {TID: "2"}, {TID: "3"}}
	AssertOrdered(mt, sent, rx)
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertOrdered_OutOfOrder(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "1"}, {TID: "2"}, {TID: "3"}}
	rx := []Received{{TID: "3"}, {TID: "1"}, {TID: "2"}}
	AssertOrdered(mt, sent, rx)
	if mt.errCount() == 0 {
		t.Fatal("expected error for out-of-order")
	}
}

// -----------------------------------------------------------------------
// AssertContentMatches
// -----------------------------------------------------------------------

func TestAssertContentMatches_PayloadExact(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a", Payload: []byte(`{"_tid":"a","key":"val"}`)}}
	rx := []Received{{TID: "a", Payload: []byte(`{"_tid":"a","key":"val"}`)}}
	AssertContentMatches(mt, sent, rx, MatchPayloadExact())
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertContentMatches_PayloadMismatch(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a", Payload: []byte(`{"_tid":"a","key":"expected"}`)}}
	rx := []Received{{TID: "a", Payload: []byte(`{"_tid":"a","key":"actual"}`)}}
	AssertContentMatches(mt, sent, rx, MatchPayloadExact())
	if mt.errCount() == 0 {
		t.Fatal("expected error for payload mismatch")
	}
}

func TestAssertContentMatches_HeaderSubset(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{
		TID:     "a",
		Headers: map[string]any{"custom": "val", "extra": "x"},
	}}
	rx := []Received{{
		TID:     "a",
		Headers: map[string]any{"custom": "val"},
	}}
	AssertContentMatches(mt, sent, rx, MatchHeadersSubset("custom"))
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertContentMatches_JSONField(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a"}}
	rx := []Received{{TID: "a", Payload: []byte(`{"factory":"A","_tid":"a"}`)}}
	AssertContentMatches(mt, sent, rx, MatchPayloadJSONField("factory", "A"))
	if mt.errCount() != 0 {
		t.Fatalf("unexpected errors: %v", mt.errors)
	}
}

func TestAssertContentMatches_MissingSent(t *testing.T) {
	mt := &mockT{}
	sent := []Expected{{TID: "a"}, {TID: "b"}}
	rx := []Received{{TID: "a"}}
	AssertContentMatches(mt, sent, rx)
	if mt.errCount() == 0 {
		t.Fatal("expected error for missing TID")
	}
}

// -----------------------------------------------------------------------
// injectPayloadTID edge cases
// -----------------------------------------------------------------------

func TestInjectPayloadTID_EmptyObject(t *testing.T) {
	out := injectPayloadTID([]byte(`{}`), "x")
	tid := extractPayloadTID(out)
	if tid != "x" {
		t.Fatalf("tid=%q, want %q from payload %q", tid, "x", out)
	}
}

func TestStripPayloadTID(t *testing.T) {
	payload := []byte(`{"_tid":"abc","key":"val"}`)
	stripped := stripPayloadTID(payload)
	tid := extractPayloadTID(stripped)
	if tid != "" {
		t.Fatalf("_tid not stripped: %q", stripped)
	}
	field := extractJSONField(stripped, "key")
	if field != "val" {
		t.Fatalf("key=%q, want %q", field, "val")
	}
}
