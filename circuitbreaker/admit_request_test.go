package circuitbreaker

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// AdmitRequest is BeforeRequestToken/AfterRequestToken with the Token inside
// the settle closure. The load-bearing property: an outcome settled
// AFTER a circuit state transition is stale evidence about a previous epoch
// and must be discarded — not applied to the current generation.
func TestBreaker_AdmitRequest_DiscardsOutcomeSettledAfterTransition(t *testing.T) {
	b := NewBreaker("admit", Config{FailureThreshold: 1, SuccessThreshold: 1}, nil)

	settle, err := b.AdmitRequest()
	require.NoError(t, err)

	// The circuit transitions while the request is in flight.
	b.ForceStateForTest(StateOpen, time.Unix(1_700_000_000, 0))

	settle(nil) // pre-transition success arriving late

	metrics := b.GetMetrics()
	assert.Equal(t, int64(1), metrics.StaleOutcomes,
		"a settle from a previous generation must be discarded as stale")
	assert.Equal(t, int64(0), metrics.TotalRequests,
		"stale outcomes must not be counted against the current generation")
	assert.Equal(t, StateOpen.String(), metrics.State,
		"a stale pre-outage success must not close the circuit")
}

func TestBreaker_AdmitRequest_SettleAppliesToSameGeneration(t *testing.T) {
	b := NewBreaker("admit-ok", Config{FailureThreshold: 2, SuccessThreshold: 1}, nil)

	settle, err := b.AdmitRequest()
	require.NoError(t, err)
	settle(errors.New("downstream failure"))

	metrics := b.GetMetrics()
	assert.Equal(t, int64(1), metrics.TotalRequests)
	assert.Equal(t, int64(1), metrics.TotalFailures)
	assert.Equal(t, 1, metrics.ConsecutiveFailures)
}

func TestBreaker_AdmitRequest_RejectsWhenOpen(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	b := NewBreaker("admit-open", Config{FailureThreshold: 1, SuccessThreshold: 1},
		nil, WithBreakerClock(fake))
	b.ForceStateForTest(StateOpen, fake.Now())

	settle, err := b.AdmitRequest()
	require.Error(t, err)
	assert.Nil(t, settle, "a rejected admission returns no settle callback")
}

// ports.AdmitCircuitBreaker is the single adapter-side admission helper: it
// prefers the generation-safe capability and falls back to the legacy pair
// for breakers that do not provide it.
func TestAdmitCircuitBreaker_PrefersAdmitterAndFallsBackToLegacy(t *testing.T) {
	t.Run("admitter preferred", func(t *testing.T) {
		b := NewBreaker("helper", Config{FailureThreshold: 1, SuccessThreshold: 1}, nil)
		settle, err := ports.AdmitCircuitBreaker(b)
		require.NoError(t, err)
		settle(nil)
		assert.Equal(t, int64(1), b.GetMetrics().TotalRequests)
	})

	t.Run("legacy fallback", func(t *testing.T) {
		legacy := &legacyOnlyBreaker{}
		settle, err := ports.AdmitCircuitBreaker(legacy)
		require.NoError(t, err)
		require.Equal(t, 1, legacy.before)
		settle(errors.New("outcome"))
		assert.Equal(t, 1, legacy.after)
	})
}

type legacyOnlyBreaker struct {
	before int
	after  int
}

func (b *legacyOnlyBreaker) BeforeRequest() error { b.before++; return nil }
func (b *legacyOnlyBreaker) AfterRequest(error)   { b.after++ }

var _ ports.CircuitBreaker = (*legacyOnlyBreaker)(nil)
