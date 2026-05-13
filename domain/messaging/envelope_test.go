package messaging_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestEnvelope_HasExpiry verifies HasExpiry is false for zero ExpiresAt and true when expiry is set.
func TestEnvelope_HasExpiry(t *testing.T) {
	e := &messaging.Envelope{}
	if e.HasExpiry() {
		t.Fatal("zero time should not have expiry")
	}
	e.ExpiresAt = time.Now().Add(time.Hour)
	if !e.HasExpiry() {
		t.Fatal("non-zero ExpiresAt should have expiry")
	}
}

// TestEnvelope_IsExpired verifies IsExpired for no expiry, past ExpiresAt, and future ExpiresAt.
func TestEnvelope_IsExpired(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(now)
	e := &messaging.Envelope{}
	if e.IsExpired(clk) {
		t.Fatal("no expiry should not be expired")
	}

	e.ExpiresAt = now.Add(-time.Second)
	if !e.IsExpired(clk) {
		t.Fatal("past ExpiresAt should be expired")
	}

	e.ExpiresAt = now.Add(time.Hour)
	if e.IsExpired(clk) {
		t.Fatal("future ExpiresAt should not be expired")
	}
}

// TestEnvelope_RemainingTTL verifies RemainingTTL is zero without expiry or when expired, and positive before ExpiresAt.
func TestEnvelope_RemainingTTL(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(now)
	e := &messaging.Envelope{}
	if r := e.RemainingTTL(clk); r != 0 {
		t.Fatalf("no expiry: expected 0, got %v", r)
	}

	e.ExpiresAt = now.Add(-time.Minute)
	if r := e.RemainingTTL(clk); r != 0 {
		t.Fatalf("expired: expected 0, got %v", r)
	}

	e.ExpiresAt = now.Add(10 * time.Second)
	if rem := e.RemainingTTL(clk); rem != 10*time.Second {
		t.Fatalf("expected remaining TTL 10s, got %v", rem)
	}
}

// TestEnvelope_Clone verifies Clone copies scalar fields and deep-copies payload and headers so mutations do not alias the original.
func TestEnvelope_Clone(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "msg-1",
		Subject:   "test",
		Payload:   []byte("hello"),
		Headers:   map[string]any{"key": "value"},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	clone := orig.Clone()

	if clone == orig {
		t.Fatal("clone should be a different pointer")
	}
	if clone.ID != orig.ID || clone.Subject() != orig.Subject() {
		t.Fatal("scalar fields should match")
	}

	// Mutating clone payload must not affect original.
	clone.Payload[0] = 'H'
	if orig.Payload[0] == 'H' {
		t.Fatal("payload was not deep-copied")
	}

	// Mutating clone headers must not affect original.
	clone.SetHeader("new", "added")
	if _, ok := orig.Headers()["new"]; ok {
		t.Fatal("headers were not deep-copied")
	}
}

// TestEnvelope_Clone_NilFields verifies Clone leaves nil Payload and Headers nil on the copy.
func TestEnvelope_Clone_NilFields(t *testing.T) {
	orig := &messaging.Envelope{ID: "msg-nil"}
	clone := orig.Clone()
	if clone.Payload != nil {
		t.Fatal("nil payload should remain nil after clone")
	}
	if clone.Headers() != nil {
		t.Fatal("nil headers should remain nil after clone")
	}
}

func TestEnvelope_Clone_DeepCopiesSliceHeaders(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"tags": []string{"a", "b"}},
	})
	clone := orig.Clone()

	cloneSlice := clone.Headers()["tags"].([]string)
	cloneSlice[0] = "mutated"

	assert.Equal(t, []string{"a", "b"}, orig.Headers()["tags"])
}

func TestEnvelope_Clone_DeepCopiesMapHeaders(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"nested": map[string]any{"k": "val"}},
	})
	clone := orig.Clone()

	nested := clone.Headers()["nested"].(map[string]any)
	nested["k"] = "changed"

	assert.Equal(t, "val", orig.Headers()["nested"].(map[string]any)["k"])
}

func TestEnvelope_Clone_DeepCopiesNestedMaps(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{
			"lvl1": map[string]any{
				"lvl2": map[string]any{"deep": "original"},
			},
		},
	})
	clone := orig.Clone()

	deep := clone.Headers()["lvl1"].(map[string]any)["lvl2"].(map[string]any)
	deep["deep"] = "mutated"

	assert.Equal(t, "original", orig.Headers()["lvl1"].(map[string]any)["lvl2"].(map[string]any)["deep"])
}

func TestEnvelope_Clone_DeepCopiesAnySlice(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{
		Headers: map[string]any{"mix": []any{1, "two", 3}},
	})
	clone := orig.Clone()

	s := clone.Headers()["mix"].([]any)
	s[0] = 999

	assert.Equal(t, []any{1, "two", 3}, orig.Headers()["mix"])
}

func TestEnvelope_Clone_NilHeaders(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: nil})
	clone := orig.Clone()
	assert.Nil(t, clone.Headers())
}

func TestEnvelope_Clone_EmptyHeaders(t *testing.T) {
	orig := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{}})
	clone := orig.Clone()

	assert.NotNil(t, clone.Headers())
	assert.Empty(t, clone.Headers())
	origPtr := reflect.ValueOf(orig.Headers()).Pointer()
	clonePtr := reflect.ValueOf(clone.Headers()).Pointer()
	assert.NotEqual(t, origPtr, clonePtr, "clone should own a distinct headers map")

	clone.SetHeader("x", 1)
	_, ok := orig.Headers()["x"]
	assert.False(t, ok)
	assert.Empty(t, orig.Headers())
}

// TestNewEnvelope_ValidID_Succeeds confirms the happy path: an explicit
// ID is preserved verbatim and the constructor returns a usable
// envelope.
func TestNewEnvelope_ValidID_Succeeds(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clk := clocktest.NewAt(now)
	e, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "env-1",
		Subject: "subj",
	}, clk.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if e.ID != "env-1" {
		t.Fatalf("ID: got %q want %q", e.ID, "env-1")
	}
	if e.Subject() != "subj" {
		t.Fatalf("Subject: got %q want %q", e.Subject(), "subj")
	}
}

// TestNewEnvelope_EmptyID_ReturnsBridgeError confirms ID is required.
// The error is matched by sentinel — adapters that need a classified
// *shared.BridgeError MUST wrap at the call site (messaging is
// stdlib-only by architectural rule).
func TestNewEnvelope_EmptyID_ReturnsBridgeError(t *testing.T) {
	clk := clocktest.NewAt(time.Now())
	_, err := messaging.NewEnvelope(messaging.EnvelopeInput{ID: ""}, clk.Now())
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	if !errorsIs(err, messaging.ErrInvalidEnvelopeID) {
		t.Fatalf("expected ErrInvalidEnvelopeID sentinel, got %v", err)
	}
	if got, want := err.Error(), "envelope ID must not be empty"; !contains(got, want) {
		t.Fatalf("error message %q should contain %q", got, want)
	}
}

// TestNewEnvelope_StripsReservedHeaders confirms reserved bridge
// metadata supplied by the caller is stripped at construction so
// untrusted ingress cannot spoof correlation_id, route_id, etc.
func TestNewEnvelope_StripsReservedHeaders(t *testing.T) {
	clk := clocktest.NewAt(time.Now())
	e, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID: "env-strip",
		Headers: map[string]any{
			"x-bridge.foo":                "spoofed",
			messaging.HeaderCorrelationID: "spoofed-corr",
			"safe":                        "kept",
		},
	}, clk.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if _, ok := e.Header("x-bridge.foo"); ok {
		t.Fatal("reserved-prefix header x-bridge.foo must be stripped")
	}
	if _, ok := e.Header(messaging.HeaderCorrelationID); ok {
		t.Fatal("reserved correlation-id header must be stripped")
	}
	if v, _ := e.Header("safe"); v != "kept" {
		t.Fatalf("non-reserved header lost: got %v", v)
	}
}

// TestNewEnvelope_StampsCreatedAtFromClock confirms a zero CreatedAt is
// stamped via the injected clock.
func TestNewEnvelope_StampsCreatedAtFromClock(t *testing.T) {
	now := time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(now)
	e, err := messaging.NewEnvelope(messaging.EnvelopeInput{ID: "env-stamp"}, clk.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if !e.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt: got %v want %v", e.CreatedAt, now)
	}
}

// TestNewEnvelope_PreservesNonZeroCreatedAt confirms a caller-supplied
// CreatedAt is preserved (e.g. rehydration from outbox snapshot).
func TestNewEnvelope_PreservesNonZeroCreatedAt(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	caller := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)
	e, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        "env-preserve",
		CreatedAt: caller,
	}, clk.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if !e.CreatedAt.Equal(caller) {
		t.Fatalf("CreatedAt should be preserved: got %v want %v", e.CreatedAt, caller)
	}
}

// TestNewEnvelope_NilClockWithZeroCreatedAt_Errors guards the
// constructor's clock-required precondition.
func TestNewEnvelope_NilClockWithZeroCreatedAt_Errors(t *testing.T) {
	_, err := messaging.NewEnvelope(messaging.EnvelopeInput{ID: "env-noclk"}, time.Time{})
	if err == nil {
		t.Fatal("expected error when CreatedAt is zero and clock is nil")
	}
	if !errorsIs(err, messaging.ErrEnvelopeClockMissing) {
		t.Fatalf("expected ErrEnvelopeClockMissing, got %v", err)
	}
}

// TestEnvelope_HeadersAccessor_LiveMapIsDocumented exercises the
// documented contract that Headers() returns the live underlying map
// (no defensive copy). Mutators MUST be used for safe mutation; the
// snapshot accessor (HeadersSnapshot) is the deep-copy alternative.
func TestEnvelope_HeadersAccessor_LiveMapIsDocumented(t *testing.T) {
	clk := clocktest.NewAt(time.Now())
	e, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "env-live",
		Headers: map[string]any{"k": "v"},
	}, clk.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	// Headers() returns the live map -- mutations leak (documented).
	got := e.Headers()
	got["leak"] = "yes"
	if v, _ := e.Header("leak"); v != "yes" {
		t.Fatal("Headers() should return the live underlying map (perf-critical contract)")
	}

	// HeadersSnapshot() is the defensive deep-copy alternative.
	snap := e.HeadersSnapshot()
	snap["snap-only"] = "isolated"
	if _, ok := e.Header("snap-only"); ok {
		t.Fatal("HeadersSnapshot() must return an isolated deep copy")
	}
}

// helpers (avoid pulling in errors/strings imports just for tests).
func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
			continue
		}
		break
	}
	return false
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
