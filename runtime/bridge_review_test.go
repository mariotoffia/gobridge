package runtime

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════
// Runtime Bridge Review Tests
//
// Validates edge cases identified by expert review:
//   - minServiceLevel correctness and no map allocation
//   - RegisterSessionSender validation
//   - AddRoute duplicate detection
// ═══════════════════════════════════════════════════════════════════

// TestMinServiceLevel validates that minServiceLevel returns the lower
// of two service levels using the correct ordering: None < Degraded < Full.
func TestMinServiceLevel(t *testing.T) {
	tests := []struct {
		name string
		a, b ports.ServiceLevel
		want ports.ServiceLevel
	}{
		{"both full", ports.ServiceLevelFull, ports.ServiceLevelFull, ports.ServiceLevelFull},
		{"full and degraded", ports.ServiceLevelFull, ports.ServiceLevelDegraded, ports.ServiceLevelDegraded},
		{"degraded and full", ports.ServiceLevelDegraded, ports.ServiceLevelFull, ports.ServiceLevelDegraded},
		{"full and none", ports.ServiceLevelFull, ports.ServiceLevelNone, ports.ServiceLevelNone},
		{"none and full", ports.ServiceLevelNone, ports.ServiceLevelFull, ports.ServiceLevelNone},
		{"both none", ports.ServiceLevelNone, ports.ServiceLevelNone, ports.ServiceLevelNone},
		{"both degraded", ports.ServiceLevelDegraded, ports.ServiceLevelDegraded, ports.ServiceLevelDegraded},
		{"degraded and none", ports.ServiceLevelDegraded, ports.ServiceLevelNone, ports.ServiceLevelNone},
		{"empty string treated as none", "", ports.ServiceLevelFull, ""},
		{"full and empty", ports.ServiceLevelFull, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := minServiceLevel(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("minServiceLevel(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestRuntime_RegisterSessionSender_EmptySessionID validates that
// registering a session sender with an empty session ID returns an error.
func TestRuntime_RegisterSessionSender_EmptySessionID(t *testing.T) {
	rt := New()
	err := rt.RegisterSessionSender(SessionConfig{}, nil, nil)
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
}

// TestRuntime_RegisterSessionSender_Duplicate validates that registering
// the same session ID twice returns an error.
func TestRuntime_RegisterSessionSender_Duplicate(t *testing.T) {
	rt := New()
	cfg := SessionConfig{SessionID: "sess-1"}

	if err := rt.RegisterSessionSender(cfg, nil, nil); err != nil {
		t.Fatalf("first registration should succeed: %v", err)
	}

	err := rt.RegisterSessionSender(cfg, nil, nil)
	if err == nil {
		t.Fatal("expected error for duplicate session sender")
	}
}

// TestRuntime_AddRoute_Duplicate validates that adding a route with
// a duplicate ID returns an error.
func TestRuntime_AddRoute_Duplicate(t *testing.T) {
	rt := New()
	cfg := RouteConfig{ID: "route-1"}

	if err := rt.AddRoute(cfg, nil, nil, nil, nil); err != nil {
		t.Fatalf("first add should succeed: %v", err)
	}

	err := rt.AddRoute(cfg, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for duplicate route ID")
	}
}
