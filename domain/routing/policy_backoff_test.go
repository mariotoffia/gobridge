package routing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestRoutePolicy_Validate_NegativeBackoff is the regression: WithDefaults
// fills only ZERO Backoff fields, so a negative interval/multiplier survives it.
// A negative MaxInterval is the dangerous one — route.retryDelay only clamps
// exponential growth behind a `> 0` MaxInterval guard, so a negative cap never
// fires and float64 growth reaches time.Duration(+Inf) (negative on amd64/arm64),
// feeding Retry a negative/near-infinite delay. Validate must reject every
// negative Backoff field with a permanent invalid-config error.
func TestRoutePolicy_Validate_NegativeBackoff(t *testing.T) {
	tests := []struct {
		name    string
		backoff routing.BackoffPolicy
		wantErr bool
	}{
		{name: "negative MaxInterval rejected", backoff: routing.BackoffPolicy{MaxInterval: -1 * time.Second}, wantErr: true},
		{name: "negative InitialInterval rejected", backoff: routing.BackoffPolicy{InitialInterval: -1 * time.Second}, wantErr: true},
		{name: "negative Multiplier rejected", backoff: routing.BackoffPolicy{Multiplier: -2.0}, wantErr: true},
		{name: "zero allowed (WithDefaults fills)", backoff: routing.BackoffPolicy{}, wantErr: false},
		{
			name:    "positive allowed",
			backoff: routing.BackoffPolicy{InitialInterval: time.Second, MaxInterval: 30 * time.Second, Multiplier: 2.0},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := routing.RoutePolicy{Backoff: tt.backoff}.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error for a negative Backoff field")
			}
			var be *shared.BridgeError
			if !errors.As(err, &be) {
				t.Fatalf("want *shared.BridgeError, got %T", err)
			}
			if be.Class != shared.ErrorPermanent {
				t.Fatalf("error class = %v, want %v", be.Class, shared.ErrorPermanent)
			}
			if be.Code != shared.ErrCodeInvalidConfig {
				t.Fatalf("error code = %v, want %v", be.Code, shared.ErrCodeInvalidConfig)
			}
		})
	}
}

// TestRoutePolicy_Validate_BackoffMultiplierBelowOne is the regression: a
// multiplier in (0,1) is not "gentler backoff", it is DECAYING backoff — every
// retry fires sooner than the one before it, so a persistently failing target is
// hammered at an ACCELERATING rate until the delay underflows to zero. Only
// >= 1 is a backoff (exactly 1 is a legal fixed interval); a zero multiplier
// still means "unset" and is filled by WithDefaults.
func TestRoutePolicy_Validate_BackoffMultiplierBelowOne(t *testing.T) {
	tests := []struct {
		name       string
		multiplier float64
		wantErr    bool
	}{
		{name: "0.5 decays and is rejected", multiplier: 0.5, wantErr: true},
		{name: "0.999 decays and is rejected", multiplier: 0.999, wantErr: true},
		{name: "1.0 is a fixed interval and is allowed", multiplier: 1.0, wantErr: false},
		{name: "2.0 is allowed", multiplier: 2.0, wantErr: false},
		{name: "zero means unset and is allowed", multiplier: 0, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := routing.RoutePolicy{Backoff: routing.BackoffPolicy{Multiplier: tt.multiplier}}.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var be *shared.BridgeError
			if !errors.As(err, &be) {
				t.Fatalf("want *shared.BridgeError, got %T (%v)", err, err)
			}
			if be.Code != shared.ErrCodeInvalidConfig || be.Class != shared.ErrorPermanent {
				t.Fatalf("error = %v/%v, want %v/%v", be.Code, be.Class,
					shared.ErrCodeInvalidConfig, shared.ErrorPermanent)
			}
		})
	}
}

// TestRoutePolicy_WithDefaults_BackoffJitterUnset is the regression: the
// recommended 0.2 equal-jitter lived ONLY in NewDefaultBackoffPolicy, so a
// policy built field-by-field (every config-loaded route) kept JitterFactor 0
// and every replica retried on the same tick. WithDefaults must fill the same
// recommended value it fills every other backoff field with.
func TestRoutePolicy_WithDefaults_BackoffJitterUnset(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()
	if p.Backoff.JitterFactor != routing.DefaultJitterFactor {
		t.Fatalf("unset JitterFactor defaulted to %v, want the recommended %v — config-loaded "+
			"routes must not silently lose the de-correlation programmatic ones get",
			p.Backoff.JitterFactor, routing.DefaultJitterFactor)
	}
	if routing.NewDefaultBackoffPolicy().JitterFactor != routing.DefaultJitterFactor {
		t.Fatal("NewDefaultBackoffPolicy and WithDefaults must use the SAME jitter default")
	}
}

// TestRoutePolicy_WithDefaults_BackoffJitterDisabled proves the tri-state: an
// operator who explicitly opts out of jitter keeps deterministic backoff. Zero
// means "unset" (defaulted above), so opting out needs its own value —
// JitterDisabled — that WithDefaults leaves alone and RetryDelay reads as "no
// jitter" (any value <= 0 disables).
func TestRoutePolicy_WithDefaults_BackoffJitterDisabled(t *testing.T) {
	p := routing.RoutePolicy{Backoff: routing.BackoffPolicy{JitterFactor: routing.JitterDisabled}}.WithDefaults()
	if p.Backoff.JitterFactor != routing.JitterDisabled {
		t.Fatalf("explicitly disabled jitter became %v; WithDefaults must not re-enable it", p.Backoff.JitterFactor)
	}
	if p.Backoff.JitterFactor > 0 {
		t.Fatal("JitterDisabled must read as 'no jitter' (<= 0) on the retry path")
	}
	if err := (routing.RoutePolicy{Backoff: routing.BackoffPolicy{JitterFactor: routing.JitterDisabled}}).Validate(); err != nil {
		t.Fatalf("JitterDisabled must be a VALID configuration, got %v", err)
	}
}

// TestRoutePolicy_Validate_BackoffJitterOutOfRange keeps every jitter value
// that is neither a fraction in [0,1] nor the explicit opt-out out of the
// runtime: RetryDelay clamps > 1 silently, so a typo like 20 (meaning "20%")
// would otherwise randomize the FULL interval with no signal.
func TestRoutePolicy_Validate_BackoffJitterOutOfRange(t *testing.T) {
	for _, jf := range []float64{-0.5, -2, 1.5, 20} {
		if err := (routing.RoutePolicy{Backoff: routing.BackoffPolicy{JitterFactor: jf}}).Validate(); err == nil {
			t.Fatalf("JitterFactor %v must be rejected: it is neither a [0,1] fraction nor JitterDisabled", jf)
		}
	}
}
