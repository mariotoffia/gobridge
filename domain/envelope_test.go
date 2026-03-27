package domain_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/stretchr/testify/assert"
)

// TestEnvelope_HasExpiry verifies HasExpiry is false for zero ExpiresAt and true when expiry is set.
func TestEnvelope_HasExpiry(t *testing.T) {
	e := &domain.Envelope{}
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
	e := &domain.Envelope{}
	if e.IsExpired() {
		t.Fatal("no expiry should not be expired")
	}

	e.ExpiresAt = time.Now().Add(-time.Second)
	if !e.IsExpired() {
		t.Fatal("past ExpiresAt should be expired")
	}

	e.ExpiresAt = time.Now().Add(time.Hour)
	if e.IsExpired() {
		t.Fatal("future ExpiresAt should not be expired")
	}
}

// TestEnvelope_RemainingTTL verifies RemainingTTL is zero without expiry or when expired, and positive before ExpiresAt.
func TestEnvelope_RemainingTTL(t *testing.T) {
	e := &domain.Envelope{}
	if r := e.RemainingTTL(); r != 0 {
		t.Fatalf("no expiry: expected 0, got %v", r)
	}

	e.ExpiresAt = time.Now().Add(-time.Minute)
	if r := e.RemainingTTL(); r != 0 {
		t.Fatalf("expired: expected 0, got %v", r)
	}

	e.ExpiresAt = time.Now().Add(10 * time.Second)
	rem := e.RemainingTTL()
	if rem <= 0 || rem > 10*time.Second {
		t.Fatalf("expected positive remaining TTL <= 10s, got %v", rem)
	}
}

// TestEnvelope_Clone verifies Clone copies scalar fields and deep-copies payload and headers so mutations do not alias the original.
func TestEnvelope_Clone(t *testing.T) {
	orig := &domain.Envelope{
		ID:        "msg-1",
		Subject:   "test",
		Payload:   []byte("hello"),
		Headers:   map[string]any{"key": "value"},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	clone := orig.Clone()

	if clone == orig {
		t.Fatal("clone should be a different pointer")
	}
	if clone.ID != orig.ID || clone.Subject != orig.Subject {
		t.Fatal("scalar fields should match")
	}

	// Mutating clone payload must not affect original.
	clone.Payload[0] = 'H'
	if orig.Payload[0] == 'H' {
		t.Fatal("payload was not deep-copied")
	}

	// Mutating clone headers must not affect original.
	clone.Headers["new"] = "added"
	if _, ok := orig.Headers["new"]; ok {
		t.Fatal("headers were not deep-copied")
	}
}

// TestEnvelope_Clone_NilFields verifies Clone leaves nil Payload and Headers nil on the copy.
func TestEnvelope_Clone_NilFields(t *testing.T) {
	orig := &domain.Envelope{ID: "msg-nil"}
	clone := orig.Clone()
	if clone.Payload != nil {
		t.Fatal("nil payload should remain nil after clone")
	}
	if clone.Headers != nil {
		t.Fatal("nil headers should remain nil after clone")
	}
}

// TestOutboxPartitionKey_WithSession validates the "SESSION#<id>" format when sessionID is non-empty.
func TestOutboxPartitionKey_WithSession(t *testing.T) {
	key := domain.OutboxPartitionKey("sess-1", "bind-1")
	if key != "SESSION#sess-1" {
		t.Fatalf("got %q, want %q", key, "SESSION#sess-1")
	}
}

// TestOutboxPartitionKey_WithBinding validates the "BINDING#<id>" format when sessionID is empty.
func TestOutboxPartitionKey_WithBinding(t *testing.T) {
	key := domain.OutboxPartitionKey("", "bind-1")
	if key != "BINDING#bind-1" {
		t.Fatalf("got %q, want %q", key, "BINDING#bind-1")
	}
}

// TestOutboxPartitionKey_Deterministic validates that the same inputs always produce the same key.
func TestOutboxPartitionKey_Deterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := domain.OutboxPartitionKey("s1", "b1")
		b := domain.OutboxPartitionKey("s1", "b1")
		if a != b {
			t.Fatalf("iteration %d: keys diverged: %q vs %q", i, a, b)
		}
	}
}

func TestEnvelope_Clone_DeepCopiesSliceHeaders(t *testing.T) {
	orig := &domain.Envelope{
		Headers: map[string]any{"tags": []string{"a", "b"}},
	}
	clone := orig.Clone()

	cloneSlice := clone.Headers["tags"].([]string)
	cloneSlice[0] = "mutated"

	assert.Equal(t, []string{"a", "b"}, orig.Headers["tags"])
}

func TestEnvelope_Clone_DeepCopiesMapHeaders(t *testing.T) {
	orig := &domain.Envelope{
		Headers: map[string]any{"nested": map[string]any{"k": "val"}},
	}
	clone := orig.Clone()

	nested := clone.Headers["nested"].(map[string]any)
	nested["k"] = "changed"

	assert.Equal(t, "val", orig.Headers["nested"].(map[string]any)["k"])
}

func TestEnvelope_Clone_DeepCopiesNestedMaps(t *testing.T) {
	orig := &domain.Envelope{
		Headers: map[string]any{
			"lvl1": map[string]any{
				"lvl2": map[string]any{"deep": "original"},
			},
		},
	}
	clone := orig.Clone()

	deep := clone.Headers["lvl1"].(map[string]any)["lvl2"].(map[string]any)
	deep["deep"] = "mutated"

	assert.Equal(t, "original", orig.Headers["lvl1"].(map[string]any)["lvl2"].(map[string]any)["deep"])
}

func TestEnvelope_Clone_DeepCopiesAnySlice(t *testing.T) {
	orig := &domain.Envelope{
		Headers: map[string]any{"mix": []any{1, "two", 3}},
	}
	clone := orig.Clone()

	s := clone.Headers["mix"].([]any)
	s[0] = 999

	assert.Equal(t, []any{1, "two", 3}, orig.Headers["mix"])
}

func TestEnvelope_Clone_NilHeaders(t *testing.T) {
	orig := &domain.Envelope{Headers: nil}
	clone := orig.Clone()
	assert.Nil(t, clone.Headers)
}

func TestEnvelope_Clone_EmptyHeaders(t *testing.T) {
	orig := &domain.Envelope{Headers: map[string]any{}}
	clone := orig.Clone()

	assert.NotNil(t, clone.Headers)
	assert.Empty(t, clone.Headers)
	origPtr := reflect.ValueOf(orig.Headers).Pointer()
	clonePtr := reflect.ValueOf(clone.Headers).Pointer()
	assert.NotEqual(t, origPtr, clonePtr, "clone should own a distinct headers map")

	clone.Headers["x"] = 1
	_, ok := orig.Headers["x"]
	assert.False(t, ok)
	assert.Empty(t, orig.Headers)
}
