package types

import (
	"math/rand"
	"time"
)

// CalculateBackoff computes the next backoff duration with jitter.
//
// Parameters:
//   - attempt: current attempt number (1-based)
//   - config: transport retry configuration
//
// Returns the backoff duration with jitter applied.
func CalculateBackoff(attempt int, config TransportRetryConfig) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	// Apply defaults if not set
	config = config.WithDefaults()

	// Calculate exponential backoff
	backoff := float64(config.InitialBackoff)
	for i := 1; i < attempt; i++ {
		backoff *= config.Multiplier
		if backoff > float64(config.MaxBackoff) {
			backoff = float64(config.MaxBackoff)
			break
		}
	}

	// Apply jitter
	backoff = addJitter(backoff, config.Jitter)

	// Cap at max
	if backoff > float64(config.MaxBackoff) {
		backoff = float64(config.MaxBackoff)
	}

	return time.Duration(backoff)
}

// CalculateAdaptiveBackoff computes backoff with error-aware adjustments.
//
// Parameters:
//   - attempt: current attempt number (1-based)
//   - config: transport retry configuration
//   - err: the error that caused the retry
//
// Infrastructure errors (DNS, connection refused) get longer backoff.
// Throttled errors use the RetryAfter hint if available.
func CalculateAdaptiveBackoff(attempt int, config TransportRetryConfig, err error) time.Duration {
	// Check for server-provided retry hint first
	if retryAfter := GetRetryAfter(err); retryAfter > 0 {
		return retryAfter
	}

	// Calculate base backoff
	backoff := CalculateBackoff(attempt, config)

	// Apply infrastructure multiplier for severe errors
	if IsInfrastructureError(err) {
		multiplier := config.InfrastructureBackoffMultiplier
		if multiplier == 0 {
			multiplier = 2.0 // Default
		}
		backoff = time.Duration(float64(backoff) * multiplier)

		// Cap at max
		if backoff > config.MaxBackoff {
			backoff = config.MaxBackoff
		}
	}

	return backoff
}

// CalculateWaitDuration computes how long to wait, respecting message TTL.
//
// Parameters:
//   - backoff: calculated backoff duration
//   - msg: the message being retried
//   - defaultTTL: default TTL if message doesn't have one
//
// Returns the wait duration (capped by remaining TTL) and whether TTL is expired.
func CalculateWaitDuration(backoff time.Duration, msg *Message, defaultTTL time.Duration) (time.Duration, bool) {
	ttl := msg.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}

	// If no TTL at all, just return the backoff
	if ttl == 0 {
		return backoff, false
	}

	// Calculate remaining TTL
	elapsed := time.Since(msg.CreatedAt)
	remaining := ttl - elapsed

	// Check if already expired
	if remaining <= 0 {
		return 0, true
	}

	// Cap wait at remaining TTL
	if backoff > remaining {
		return remaining, false
	}

	return backoff, false
}

// addJitter adds randomness to prevent thundering herd.
// jitterFactor is between 0.0 (no jitter) and 1.0 (full jitter).
func addJitter(value float64, jitterFactor float64) float64 {
	if jitterFactor <= 0 {
		return value
	}
	if jitterFactor > 1 {
		jitterFactor = 1
	}

	// Calculate jitter range
	jitterRange := value * jitterFactor

	// Random value between -jitterRange/2 and +jitterRange/2
	jitter := (rand.Float64() - 0.5) * jitterRange

	result := value + jitter
	if result < 0 {
		result = 0
	}

	return result
}
