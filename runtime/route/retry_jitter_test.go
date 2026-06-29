package route

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestRetryDelay_JitterZeroIsDeterministic verifies finding #27: a zero
// JitterFactor reproduces the exact deterministic exponential backoff,
// even when the injected randomness source would otherwise shift it.
func TestRetryDelay_JitterZeroIsDeterministic(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		JitterFactor:    0,
	}}
	// A source that WOULD move the delay if jitter were applied.
	randFloat := func() float64 { return 0.5 }
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		attempt := i + 1
		if got := retryDelay(policy, attempt, errors.New("transient"), randFloat); got != w {
			t.Fatalf("attempt %d: got %v, want %v (jitter=0 must be deterministic)", attempt, got, w)
		}
	}
}

// TestRetryDelay_JitterAppliesEqualJitterBounds verifies the equal-jitter
// formula: delay = d*(1-jf) + rand[0, d*jf), so the result lies in
// [d*(1-jf), d).
func TestRetryDelay_JitterAppliesEqualJitterBounds(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		JitterFactor:    0.5,
	}}
	const d = 4 * time.Second // attempt 3 -> uncapped exponential
	const jf = 0.5
	lowerBound := time.Duration(float64(d) * (1 - jf))

	// randFloat = 0 -> exactly the lower bound.
	if low := retryDelay(policy, 3, errors.New("transient"), func() float64 { return 0 }); low != lowerBound {
		t.Fatalf("randFloat=0: got %v, want %v", low, lowerBound)
	}
	// randFloat -> ~1 -> just under d (upper bound exclusive).
	high := retryDelay(policy, 3, errors.New("transient"), func() float64 { return 0.9999999 })
	if high < lowerBound || high >= d {
		t.Fatalf("randFloat~1: got %v, want within [%v, %v)", high, lowerBound, d)
	}
}

// TestRetryDelay_JitterNeverExceedsMaxInterval verifies jitter applied to
// a capped delay never overshoots MaxInterval and never goes negative.
func TestRetryDelay_JitterNeverExceedsMaxInterval(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		JitterFactor:    1.0,
	}}
	// attempt 8 -> capped exponential d = MaxInterval (30s).
	for _, rf := range []float64{0, 0.5, 0.9999999} {
		got := retryDelay(policy, 8, errors.New("transient"), func() float64 { return rf })
		if got > 30*time.Second {
			t.Fatalf("randFloat=%v: got %v, must never exceed MaxInterval 30s", rf, got)
		}
		if got < 0 {
			t.Fatalf("randFloat=%v: got negative delay %v", rf, got)
		}
	}
}

// TestRetryDelay_RetryAfterIsNotJittered verifies a broker-provided
// RetryAfter hint is returned verbatim, never randomized.
func TestRetryDelay_RetryAfterIsNotJittered(t *testing.T) {
	policy := routing.RoutePolicy{Backoff: routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		JitterFactor:    0.5,
	}}
	sendErr := shared.ErrThrottled.WithRetryAfter(5 * time.Second)
	if got := retryDelay(policy, 2, sendErr, func() float64 { return 0.1 }); got != 5*time.Second {
		t.Fatalf("broker RetryAfter must be returned as-is, got %v", got)
	}
}
