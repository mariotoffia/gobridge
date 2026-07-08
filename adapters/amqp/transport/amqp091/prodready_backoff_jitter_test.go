// ═══════════════════════════════════════════════
// Production-readiness remediation test: receiver retry backoff jitter (LOW).
//
// The pure receiverBackoff schedule made every receiver that fails in
// lockstep (e.g. a broker bounce) re-consume on the same beat — a
// synchronized thundering herd. jitteredBackoff applies ±25% jitter at the
// call site while keeping receiverBackoff itself exactly assertable.
// ═══════════════════════════════════════════════
package amqp091

import (
	"testing"
	"time"
)

// TestReceiver_JitteredBackoff_AppliesQuarterJitter pins the ±25% jitter
// envelope and proves jitter is actually applied at the call site.
//
// Counterfactual (call receiverBackoff directly at the call site): every
// randFloat yields the same base, so the extremes-differ assertion fails.
func TestReceiver_JitteredBackoff_AppliesQuarterJitter(t *testing.T) {
	const failures = 3
	base := receiverBackoff(failures) // 100ms<<2 = 400ms, below the 5s cap

	cases := []struct {
		name   string
		rf     float64
		factor float64
	}{
		{"low extreme -> -25%", 0.0, 0.75},
		{"midpoint -> no jitter", 0.5, 1.0},
		{"high extreme -> +25%", 1.0, 1.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Receiver{randFloat: func() float64 { return tc.rf }}
			got := r.jitteredBackoff(failures)
			want := time.Duration(float64(base) * tc.factor)
			if got != want {
				t.Fatalf("jitteredBackoff(%d) rf=%v = %v, want %v", failures, tc.rf, got, want)
			}
		})
	}

	// Teeth: the extremes must move OFF the base — otherwise no jitter is
	// being applied (the counterfactual yields base for every randFloat).
	low := (&Receiver{randFloat: func() float64 { return 0.0 }}).jitteredBackoff(failures)
	high := (&Receiver{randFloat: func() float64 { return 1.0 }}).jitteredBackoff(failures)
	if low == base || high == base {
		t.Fatalf("jitter not applied: low=%v high=%v both must differ from base=%v", low, high, base)
	}
	if low >= high {
		t.Fatalf("jitter direction wrong: low=%v must be < high=%v", low, high)
	}
}

// TestReceiver_JitteredBackoff_NilRandFloat_StaysInEnvelope proves the
// production default (nil randFloat -> rand.Float64) keeps the backoff
// within the ±25% envelope for the whole schedule, and never returns a
// non-positive delay (which would hot-loop).
func TestReceiver_JitteredBackoff_NilRandFloat_StaysInEnvelope(t *testing.T) {
	r := &Receiver{} // nil randFloat -> rand.Float64
	for failures := 1; failures <= 12; failures++ {
		base := receiverBackoff(failures)
		lo := time.Duration(float64(base) * 0.75)
		hi := time.Duration(float64(base) * 1.25)
		for i := 0; i < 200; i++ {
			got := r.jitteredBackoff(failures)
			if got <= 0 {
				t.Fatalf("failures=%d: jittered backoff %v must be positive", failures, got)
			}
			if got < lo || got > hi {
				t.Fatalf("failures=%d: jittered backoff %v outside [%v,%v]", failures, got, lo, hi)
			}
		}
	}
}
