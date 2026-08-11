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
