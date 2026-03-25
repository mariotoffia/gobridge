package retry

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// calculateBackoff calculates the backoff duration for the given attempt.
func calculateBackoff(policy types.RetryPolicy, attempt int) time.Duration {
	if policy.InitialBackoff == 0 {
		policy.InitialBackoff = time.Second
	}
	if policy.MaxBackoff == 0 {
		policy.MaxBackoff = 30 * time.Second
	}
	if policy.BackoffMultiplier == 0 {
		policy.BackoffMultiplier = 2.0
	}

	// Calculate exponential backoff
	backoff := float64(policy.InitialBackoff) * math.Pow(policy.BackoffMultiplier, float64(attempt-1))

	// Apply max backoff
	if backoff > float64(policy.MaxBackoff) {
		backoff = float64(policy.MaxBackoff)
	}

	// Apply jitter
	if policy.Jitter > 0 {
		jitter := backoff * policy.Jitter
		backoff = backoff - jitter + (rand.Float64() * jitter * 2)
	}

	return time.Duration(backoff)
}

// nextRetryTime calculates when the next retry should occur.
func nextRetryTime(policy types.RetryPolicy, attempt int) time.Time {
	backoff := calculateBackoff(policy, attempt)
	return time.Now().Add(backoff)
}
