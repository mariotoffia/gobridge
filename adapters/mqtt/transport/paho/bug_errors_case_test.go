package paho

import (
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-5: MQTT containsAny Case Sensitivity
//
// containsAny uses exact byte comparison. Mixed-case error messages are
// not matched, causing MapError to misclassify connection errors.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug5_ContainsAny_CaseSensitive exposes case-sensitive matching.
func TestBug5_ContainsAny_CaseSensitive(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		substrs  []string
		expected bool
		isBug    bool
	}{
		{"exact match", "connection refused", []string{"connection refused"}, true, false},
		{"exact multi", "connection refused extra", []string{"connection refused"}, true, false},
		{"empty input", "", []string{"connection refused"}, false, false},
		{"short input", "short", []string{"connection refused"}, false, false},

		// FIX VERIFIED: mixed-case now matches (case-insensitive).
		{"Title Case", "Connection Refused", []string{"connection refused"}, true, false},
		{"UPPER CASE", "CONNECTION REFUSED", []string{"connection refused"}, true, false},
		{"embedded Title", "error: Connection refused by peer", []string{"connection refused"}, true, false},
		{"no route Title", "no Route To Host", []string{"no route to host"}, true, false},
		{"network Title", "Network Unreachable", []string{"network unreachable"}, true, false},
		{"server Title", "Server Unavailable", []string{"server unavailable"}, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containsAny(tc.input, tc.substrs...)
			if got != tc.expected {
				t.Errorf("containsAny(%q, %v) = %v, want %v", tc.input, tc.substrs, got, tc.expected)
			}
			if tc.isBug {
				t.Logf("BUG-5 EVIDENCE: containsAny(%q, %v) = false (should be true)", tc.input, tc.substrs)
			}
		})
	}
}

// TestBug5_MapError_MissesUpperCaseConnectionRefused exposes that
// MapError misclassifies mixed-case connection errors.
func TestBug5_MapError_MissesUpperCaseConnectionRefused(t *testing.T) {
	tests := []struct {
		name        string
		errMsg      string
		wantCode    domain.ErrorCode // what the buggy code actually returns
		correctCode domain.ErrorCode // what it should return
	}{
		{
			"exact lowercase",
			"connection refused",
			domain.ErrConnectionLost.Code,
			domain.ErrConnectionLost.Code,
		},
		{
			"Title Case (fixed)",
			"Connection Refused",
			domain.ErrConnectionLost.Code, // fixed: case-insensitive match
			domain.ErrConnectionLost.Code,
		},
		{
			"embedded Title (fixed)",
			"dial tcp: Connection Refused by peer",
			domain.ErrConnectionLost.Code, // fixed: case-insensitive match
			domain.ErrConnectionLost.Code,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := MapError(errors.New(tc.errMsg))
			if be.Code != tc.wantCode {
				t.Errorf("MapError(%q).Code = %s, want %s", tc.errMsg, be.Code, tc.wantCode)
			}
			if tc.wantCode != tc.correctCode {
				t.Logf("BUG-5 EVIDENCE: MapError(%q) returns %s instead of %s",
					tc.errMsg, be.Code, tc.correctCode)
			}
		})
	}
}
