// ═══════════════════════════════════════════════
// Config Validation & Edge Case Tests
//
// Validates input validation, overflow guards, negative
// duration rejection, and NaN/Inf handling in config parsing.
// ═══════════════════════════════════════════════
package amqp091

import (
	"math"
	"testing"
	"time"
)

// TestOptInt_LargeFloat64 validates that float64 values exceeding
// platform int range are rejected.
func TestOptInt_LargeFloat64(t *testing.T) {
	m := map[string]any{"big": float64(1e19)}
	_, ok := optInt(m, "big")
	if ok {
		t.Fatal("expected very large float64 to be rejected")
	}
}

// TestOptInt_Float64NaN validates NaN is rejected.
func TestOptInt_Float64NaN(t *testing.T) {
	m := map[string]any{"nan": math.NaN()}
	_, ok := optInt(m, "nan")
	if ok {
		t.Fatal("expected NaN to be rejected")
	}
}

// TestOptInt_Float64Inf validates Inf is rejected.
func TestOptInt_Float64Inf(t *testing.T) {
	m := map[string]any{"inf": math.Inf(1)}
	_, ok := optInt(m, "inf")
	if ok {
		t.Fatal("expected +Inf to be rejected")
	}
}

// TestOptInt_Float64NegInf validates -Inf is rejected.
func TestOptInt_Float64NegInf(t *testing.T) {
	m := map[string]any{"neginf": math.Inf(-1)}
	_, ok := optInt(m, "neginf")
	if ok {
		t.Fatal("expected -Inf to be rejected")
	}
}

// TestOptInt_ValidCases validates normal int extraction still works.
func TestOptInt_ValidCases(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want int
	}{
		{"int_42", 42, 42},
		{"int64_100", int64(100), 100},
		{"float64_7", float64(7), 7},
		{"int_zero", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{"k": tt.val}
			got, ok := optInt(m, "k")
			if !ok {
				t.Fatalf("optInt returned ok=false for valid input %v", tt.val)
			}
			if got != tt.want {
				t.Fatalf("optInt = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOptDuration_NegativeInt validates negative int is rejected.
func TestOptDuration_NegativeInt(t *testing.T) {
	m := map[string]any{"d": -5}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative int duration to be rejected")
	}
}

// TestOptDuration_NegativeInt64 validates negative int64 is rejected.
func TestOptDuration_NegativeInt64(t *testing.T) {
	m := map[string]any{"d": int64(-1)}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative int64 duration to be rejected")
	}
}

// TestOptDuration_NegativeFloat64 validates negative float64 is rejected.
func TestOptDuration_NegativeFloat64(t *testing.T) {
	m := map[string]any{"d": -1.5}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative float64 duration to be rejected")
	}
}

// TestOptDuration_NaN validates NaN is rejected.
func TestOptDuration_NaN(t *testing.T) {
	m := map[string]any{"d": math.NaN()}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected NaN duration to be rejected")
	}
}

// TestOptDuration_NegativeDuration validates negative time.Duration is rejected.
func TestOptDuration_NegativeDuration(t *testing.T) {
	m := map[string]any{"d": -1 * time.Second}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative time.Duration to be rejected")
	}
}

// TestOptDuration_NegativeString validates negative string durations are rejected.
func TestOptDuration_NegativeString(t *testing.T) {
	m := map[string]any{"d": "-5s"}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative string duration to be rejected")
	}
}

// TestOptDuration_ValidCases validates normal duration extraction still works.
func TestOptDuration_ValidCases(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want time.Duration
	}{
		{"duration_5s", 5 * time.Second, 5 * time.Second},
		{"string_10s", "10s", 10 * time.Second},
		{"int_3", 3, 3 * time.Second},
		{"int64_7", int64(7), 7 * time.Second},
		{"float64_2.5", 2.5, time.Duration(2.5 * float64(time.Second))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{"k": tt.val}
			got, ok := optDuration(m, "k")
			if !ok {
				t.Fatalf("optDuration returned ok=false for valid input %v", tt.val)
			}
			if got != tt.want {
				t.Fatalf("optDuration = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildTLSConfig_EnableFalseWithCerts validates Enable=false returns nil
// even when cert paths are specified.
func TestBuildTLSConfig_EnableFalseWithCerts(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{
		Enable:     false,
		CACertFile: "/some/ca.pem",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when Enable=false")
	}
}
