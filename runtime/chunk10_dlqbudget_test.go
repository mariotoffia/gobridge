package runtime

import (
	"testing"
	"time"
)

// TestDLQWriteBudget_MatchesRouterWiring pins validateTimeouts' DLQ-write budget
// to the actual DLQ router wiring in bridge_start.go (WriteTimeout 5s ×
// WriteMaxAttempts 2) plus the DLQ router's 500ms default inter-attempt backoff.
// The budget is a hand-computed constant spanning two files, so if any of those
// wiring values change, the validator's worst-case source-hold bound goes stale
// (silently under- or over-counting). This fails on that drift and forces a
// deliberate update of dlqWriteBudget (F4 / adversarial finding #5).
func TestDLQWriteBudget_MatchesRouterWiring(t *testing.T) {
	const (
		writeTimeout     = 5 * time.Second        // bridge_start.go DLQ router
		writeMaxAttempts = 2                      // bridge_start.go DLQ router
		routerBackoff    = 500 * time.Millisecond // dlq router default InitialInterval
	)
	// N attempts spend N write timeouts and (N-1) inter-attempt backoffs.
	want := time.Duration(writeMaxAttempts)*writeTimeout + time.Duration(writeMaxAttempts-1)*routerBackoff
	if want != dlqWriteBudget {
		t.Fatalf("dlqWriteBudget %s drifted from the DLQ router wiring (recomputed %s); "+
			"update the constant or the wiring deliberately", dlqWriteBudget, want)
	}
	if dlqWriteBudget != 10500*time.Millisecond {
		t.Fatalf("dlqWriteBudget = %s, want 10.5s", dlqWriteBudget)
	}
}
