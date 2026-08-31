package route

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// Baseline for the retry hot path. Every config-loaded route now carries the
// recommended equal-jitter by default, so the randomized branch — previously
// reached only by programmatic policies — runs on every transient failure of
// every route. These pin what that costs against the deterministic path an
// operator gets by opting out.

var errBenchSend = errors.New("transient target failure")

// BenchmarkRetryDelay measures one backoff computation at the attempt depths a
// real route reaches: the first retry, mid-ladder, and past the interval cap
// where the exponential loop breaks early.
func BenchmarkRetryDelay(b *testing.B) {
	jittered := routing.RoutePolicy{}.WithDefaults()
	deterministic := routing.RoutePolicy{
		Backoff: routing.BackoffPolicy{JitterFactor: routing.JitterDisabled},
	}.WithDefaults()

	for _, tc := range []struct {
		name   string
		policy routing.RoutePolicy
	}{
		{name: "jittered", policy: jittered},
		{name: "deterministic", policy: deterministic},
	} {
		for _, attempt := range []int{1, 5, 20} {
			b.Run(fmt.Sprintf("%s/attempt=%d", tc.name, attempt), func(b *testing.B) {
				b.ReportAllocs()
				var sink time.Duration
				for b.Loop() {
					sink = RetryDelay(tc.policy, attempt, errBenchSend)
				}
				_ = sink
			})
		}
	}
}

// BenchmarkRetryDelay_FullLadder walks a message from its first transient
// failure to the replay cap — the sequence a route actually executes for one
// poisoned message — so the per-message cost of the retry ladder is visible
// rather than the per-call cost alone.
func BenchmarkRetryDelay_FullLadder(b *testing.B) {
	policy := routing.RoutePolicy{MaxReplayAttempts: 10}.WithDefaults()
	b.ReportAllocs()
	var sink time.Duration
	for b.Loop() {
		for attempt := 1; attempt <= policy.MaxReplayAttempts; attempt++ {
			sink += RetryDelay(policy, attempt, errBenchSend)
		}
	}
	_ = sink
}

// BenchmarkRoutePolicy_WithDefaults measures policy normalisation, which now
// resolves the jitter tri-state as well. It runs once per route construction
// and once per route validation, so a reconfiguration of a large blueprint
// pays it per route.
func BenchmarkRoutePolicy_WithDefaults(b *testing.B) {
	b.ReportAllocs()
	var sink routing.RoutePolicy
	for b.Loop() {
		sink = routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox}.WithDefaults()
	}
	_ = sink
}
