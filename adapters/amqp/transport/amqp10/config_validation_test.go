// ═══════════════════════════════════════════════
// Config Validation & Edge Case Tests
//
// Validates input validation, uint32 overflow guards,
// negative duration rejection, LinkCredit bounds,
// and NaN/Inf handling in AMQP 1.0 config parsing.
// ═══════════════════════════════════════════════
package amqp10

import (
	"math"
	"testing"
	"time"
)

// TestOptUint32_OverflowInt validates int values > MaxUint32 are rejected.
func TestOptUint32_OverflowInt(t *testing.T) {
	m := map[string]any{"v": int(math.MaxUint32 + 1)}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected int > MaxUint32 to be rejected")
	}
}

// TestOptUint32_OverflowInt64 validates int64 values > MaxUint32 are rejected.
func TestOptUint32_OverflowInt64(t *testing.T) {
	m := map[string]any{"v": int64(math.MaxUint32 + 1)}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected int64 > MaxUint32 to be rejected")
	}
}

// TestOptUint32_OverflowFloat64 validates float64 values > MaxUint32 are rejected.
func TestOptUint32_OverflowFloat64(t *testing.T) {
	m := map[string]any{"v": float64(math.MaxUint32 + 1)}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected float64 > MaxUint32 to be rejected")
	}
}

// TestOptUint32_NaN validates NaN is rejected.
func TestOptUint32_NaN(t *testing.T) {
	m := map[string]any{"v": math.NaN()}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected NaN to be rejected")
	}
}

// TestOptUint32_Inf validates +Inf is rejected.
func TestOptUint32_Inf(t *testing.T) {
	m := map[string]any{"v": math.Inf(1)}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected +Inf to be rejected")
	}
}

// TestOptUint32_NegativeInt validates negative int is rejected.
func TestOptUint32_NegativeInt(t *testing.T) {
	m := map[string]any{"v": -1}
	_, ok := optUint32(m, "v")
	if ok {
		t.Fatal("expected negative int to be rejected")
	}
}

// TestOptUint32_ValidCases validates normal uint32 extraction.
func TestOptUint32_ValidCases(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want uint32
	}{
		{"int_42", 42, 42},
		{"int32_100", int32(100), 100},
		{"int64_200", int64(200), 200},
		{"uint32_300", uint32(300), 300},
		{"float64_7", float64(7), 7},
		{"max_uint32", uint32(math.MaxUint32), math.MaxUint32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{"k": tt.val}
			got, ok := optUint32(m, "k")
			if !ok {
				t.Fatalf("optUint32 returned ok=false for valid input %v", tt.val)
			}
			if got != tt.want {
				t.Fatalf("optUint32 = %d, want %d", got, tt.want)
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

// TestOptDuration_NaN validates NaN is rejected.
func TestOptDuration_NaN(t *testing.T) {
	m := map[string]any{"d": math.NaN()}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected NaN duration to be rejected")
	}
}

// TestOptDuration_NegativeDuration validates negative time.Duration rejected.
func TestOptDuration_NegativeDuration(t *testing.T) {
	m := map[string]any{"d": -1 * time.Second}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative time.Duration to be rejected")
	}
}

// TestOptDuration_NegativeString validates negative string durations rejected.
func TestOptDuration_NegativeString(t *testing.T) {
	m := map[string]any{"d": "-5s"}
	_, ok := optDuration(m, "d")
	if ok {
		t.Fatal("expected negative string duration to be rejected")
	}
}

// TestReceiverConfig_LinkCreditOverflow validates that LinkCredit
// exceeding int32 max is rejected during validation.
func TestReceiverConfig_LinkCreditOverflow(t *testing.T) {
	cfg := ReceiverConfig{
		Address:    "test-queue",
		LinkCredit: math.MaxInt32 + 1,
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected validation error for oversized LinkCredit")
	}
}

// TestReceiverConfig_ValidLinkCredit validates normal LinkCredit passes.
func TestReceiverConfig_ValidLinkCredit(t *testing.T) {
	cfg := ReceiverConfig{
		Address:    "test-queue",
		LinkCredit: 100,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

// TestSenderConfigFromOptions_NilMap validates nil map doesn't panic.
func TestSenderConfigFromOptions_NilMap(t *testing.T) {
	cfg := SenderConfigFromOptions(nil)
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("expected default timeout, got %v", cfg.Timeout)
	}
}

// TestReceiverConfigFromOptions_NilMap validates nil map doesn't panic.
func TestReceiverConfigFromOptions_NilMap(t *testing.T) {
	cfg := ReceiverConfigFromOptions(nil)
	if cfg.Address != "" {
		t.Fatal("expected empty address from nil map")
	}
}

// TestSessionOptionsFromMap_MissingAddress validates address is required.
func TestSessionOptionsFromMap_MissingAddress(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing address")
	}
}
