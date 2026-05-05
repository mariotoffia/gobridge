package paho

import (
	"math"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-SEV: SessionExpiryInterval negative / overflow inputs silently
//          coerce to bogus uint32 values via two's-complement wrap.
//
// Defect:
//
//	SessionOptionsFromMap previously cast int / int64 / float64 inputs
//	to uint32 with no range check. A stray "-1" turned into
//	0xFFFFFFFF (4 294 967 295) seconds — which MQTT v5 interprets as
//	"never expire". Operators expecting "session_expiry_interval = -1"
//	to be rejected (or to mean "use default") instead got persistent
//	sessions on the broker, leaking client state forever.
//
//	NaN / +Inf / -Inf float inputs also produced undefined uint32
//	values via Go's float-to-int conversion semantics.
//
// Fix:
//
//	Validate the input range. Reject negatives, NaN, and ±Inf with a
//	descriptive error so misconfiguration is loud at startup rather
//	than silent at runtime.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugSEV_NegativeIntRejected exposes the int wrap-around: -1 must
// not silently become 0xFFFFFFFF.
func TestBugSEV_NegativeIntRejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": -1,
	})
	if err == nil {
		t.Fatal("BUG-SEV: SessionOptionsFromMap must reject negative session_expiry_interval")
	}
	if !strings.Contains(err.Error(), "session_expiry_interval") {
		t.Fatalf("BUG-SEV: error must mention the offending key, got %v", err)
	}
}

// TestBugSEV_NegativeInt64Rejected exposes the int64 wrap-around path.
func TestBugSEV_NegativeInt64Rejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": int64(-42),
	})
	if err == nil {
		t.Fatal("BUG-SEV: int64 negative must be rejected")
	}
}

// TestBugSEV_NegativeFloat64Rejected exposes the float wrap path.
func TestBugSEV_NegativeFloat64Rejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": float64(-1.5),
	})
	if err == nil {
		t.Fatal("BUG-SEV: float64 negative must be rejected")
	}
}

// TestBugSEV_NaNRejected validates that NaN is rejected.
func TestBugSEV_NaNRejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": math.NaN(),
	})
	if err == nil {
		t.Fatal("BUG-SEV: NaN must be rejected")
	}
}

// TestBugSEV_InfRejected validates that +Inf / -Inf are rejected.
func TestBugSEV_InfRejected(t *testing.T) {
	for _, v := range []float64{math.Inf(1), math.Inf(-1)} {
		_, err := SessionOptionsFromMap(map[string]any{
			"session_expiry_interval": v,
		})
		if err == nil {
			t.Fatalf("BUG-SEV: %v must be rejected", v)
		}
	}
}

// TestBugSEV_OverflowInt64Rejected verifies that values exceeding
// uint32 max are rejected.
func TestBugSEV_OverflowInt64Rejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": int64(math.MaxUint32) + 1,
	})
	if err == nil {
		t.Fatal("BUG-SEV: value > MaxUint32 must be rejected")
	}
}

// TestBugSEV_WrongTypeRejected verifies that a non-number value type
// is rejected.
func TestBugSEV_WrongTypeRejected(t *testing.T) {
	_, err := SessionOptionsFromMap(map[string]any{
		"session_expiry_interval": "forever",
	})
	if err == nil {
		t.Fatal("BUG-SEV: non-number type must be rejected")
	}
}

// TestBugSEV_BoundaryValuesAccepted verifies the legal extremes.
func TestBugSEV_BoundaryValuesAccepted(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want uint32
	}{
		{"zero int", 0, 0},
		{"one int", 1, 1},
		{"max uint32 via int64", int64(math.MaxUint32), math.MaxUint32},
		{"reasonable float64", float64(3600), 3600},
		{"explicit uint32", uint32(7200), 7200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := SessionOptionsFromMap(map[string]any{
				"session_expiry_interval": tc.val,
			})
			if err != nil {
				t.Fatalf("unexpected error for %v: %v", tc.val, err)
			}
			if opts.SessionExpiryInterval != tc.want {
				t.Fatalf("SessionExpiryInterval = %d, want %d", opts.SessionExpiryInterval, tc.want)
			}
		})
	}
}
