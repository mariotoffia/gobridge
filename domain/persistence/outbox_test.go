package persistence_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// TestOutboxPartitionKey_WithSession validates the "SESSION#<id>" format when sessionID is non-empty.
func TestOutboxPartitionKey_WithSession(t *testing.T) {
	key := persistence.OutboxPartitionKey("sess-1", "bind-1")
	if key != "SESSION#sess-1" {
		t.Fatalf("got %q, want %q", key, "SESSION#sess-1")
	}
}

// TestOutboxPartitionKey_WithBinding validates the "BINDING#<id>" format when sessionID is empty.
func TestOutboxPartitionKey_WithBinding(t *testing.T) {
	key := persistence.OutboxPartitionKey("", "bind-1")
	if key != "BINDING#bind-1" {
		t.Fatalf("got %q, want %q", key, "BINDING#bind-1")
	}
}

// TestOutboxPartitionKey_Deterministic validates that the same inputs always produce the same key.
func TestOutboxPartitionKey_Deterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		a := persistence.OutboxPartitionKey("s1", "b1")
		b := persistence.OutboxPartitionKey("s1", "b1")
		if a != b {
			t.Fatalf("iteration %d: keys diverged: %q vs %q", i, a, b)
		}
	}
}
