package domain_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

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
