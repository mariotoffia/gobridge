package persistence_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// TestLeaseToken_ZeroValue validates that a zero-value LeaseToken has Version 0 and empty Owner.
func TestLeaseToken_ZeroValue(t *testing.T) {
	var tok persistence.LeaseToken
	if tok.Version != 0 {
		t.Fatalf("zero-value Version: got %d, want 0", tok.Version)
	}
	if tok.Owner != "" {
		t.Fatalf("zero-value Owner: got %q, want empty", tok.Owner)
	}
}

// TestLeaseInfo_ZeroValue validates that a zero-value LeaseInfo has zero time ExpiresAt and empty string fields.
func TestLeaseInfo_ZeroValue(t *testing.T) {
	var info persistence.LeaseInfo
	if info.LeaseID != "" {
		t.Fatalf("zero-value LeaseID: got %q, want empty", info.LeaseID)
	}
	if info.Owner != "" {
		t.Fatalf("zero-value Owner: got %q, want empty", info.Owner)
	}
	if info.Version != 0 {
		t.Fatalf("zero-value Version: got %d, want 0", info.Version)
	}
	if !info.ExpiresAt.IsZero() {
		t.Fatalf("zero-value ExpiresAt should be zero time, got %v", info.ExpiresAt)
	}
}

// TestLeaseInfo_Fields validates that LeaseInfo fields can be assigned and retrieved correctly.
func TestLeaseInfo_Fields(t *testing.T) {
	exp := time.Now().Add(5 * time.Minute)
	info := persistence.LeaseInfo{
		LeaseID:   "lease-abc",
		Owner:     "instance-1",
		Version:   42,
		ExpiresAt: exp,
	}
	if info.LeaseID != "lease-abc" {
		t.Fatalf("LeaseID: got %q, want %q", info.LeaseID, "lease-abc")
	}
	if info.Owner != "instance-1" {
		t.Fatalf("Owner: got %q, want %q", info.Owner, "instance-1")
	}
	if info.Version != 42 {
		t.Fatalf("Version: got %d, want 42", info.Version)
	}
	if !info.ExpiresAt.Equal(exp) {
		t.Fatalf("ExpiresAt: got %v, want %v", info.ExpiresAt, exp)
	}
}
