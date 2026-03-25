// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Errors Internal Unit Tests
//
// # Tests for unexported helper functions in errors.go
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ EI001│ containsAny no match                   │ PASS     │
// │ EI002│ containsAny first match                │ PASS     │
// │ EI003│ containsAny last match                 │ PASS     │
// │ EI004│ containsAny empty string               │ PASS     │
// │ EI005│ containsAny empty substrings           │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// containsAny Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestContainsAny_NoMatch validates no match returns false.
func TestContainsAny_NoMatch(t *testing.T) {
	s := "connection error: timeout"
	result := containsAny(s, "refused", "unreachable")
	assert.False(t, result, "should not match when no substring found")
}

// TestContainsAny_FirstMatch validates first substring match returns true.
func TestContainsAny_FirstMatch(t *testing.T) {
	s := "connection refused by remote host"
	result := containsAny(s, "connection refused", "network unreachable")
	assert.True(t, result, "should match first substring")
}

// TestContainsAny_LastMatch validates last substring match returns true.
func TestContainsAny_LastMatch(t *testing.T) {
	s := "network unreachable: no route to host"
	result := containsAny(s, "connection refused", "no route to host")
	assert.True(t, result, "should match last substring")
}

// TestContainsAny_EmptyString validates empty string returns false.
func TestContainsAny_EmptyString(t *testing.T) {
	s := ""
	result := containsAny(s, "connection refused", "network unreachable")
	assert.False(t, result, "empty string should not match")
}

// TestContainsAny_EmptySubstrings validates empty substrings returns false.
func TestContainsAny_EmptySubstrings(t *testing.T) {
	s := "connection error"
	result := containsAny(s)
	assert.False(t, result, "no substrings should not match")
}

// TestContainsAny_PartialMatch validates partial substring match.
func TestContainsAny_PartialMatch(t *testing.T) {
	s := "broker unavailable at this time"
	result := containsAny(s, "unavailable", "timeout")
	assert.True(t, result, "should match partial substring")
}

// TestContainsAny_CaseSensitive validates case-sensitive matching.
func TestContainsAny_CaseSensitive(t *testing.T) {
	s := "Connection Refused"
	result := containsAny(s, "connection refused")
	assert.False(t, result, "should be case-sensitive")
}
