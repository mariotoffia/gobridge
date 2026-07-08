package routing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestRoutePolicy_WithDefaults verifies WithDefaults applies package defaults to all zero-valued RoutePolicy fields.
func TestRoutePolicy_WithDefaults(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != routing.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight: got %d, want %d", p.MaxInFlight, routing.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts: got %d, want %d", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
	}
	if p.ReplayBudget != routing.DefaultReplayBudget {
		t.Fatalf("ReplayBudget: got %v, want %v", p.ReplayBudget, routing.DefaultReplayBudget)
	}
	if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth: got %d, want %d", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
	}
	if p.Backoff.InitialInterval != 1*time.Second {
		t.Fatalf("Backoff.InitialInterval: got %v, want 1s", p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval != 30*time.Second {
		t.Fatalf("Backoff.MaxInterval: got %v, want 30s", p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier != 2.0 {
		t.Fatalf("Backoff.Multiplier: got %v, want 2.0", p.Backoff.Multiplier)
	}
	if p.OnExpired != routing.ExpiredDLQ {
		t.Fatalf("OnExpired: got %s, want %s", p.OnExpired, routing.ExpiredDLQ)
	}
	if p.OnPermanentFailure != routing.FailureDLQ {
		t.Fatalf("OnPermanentFailure: got %s, want %s", p.OnPermanentFailure, routing.FailureDLQ)
	}
	if p.OnFiltered != routing.FilteredDrop {
		t.Fatalf("OnFiltered: got %s, want %s (filter-drops default to drop, NOT the DLQ)", p.OnFiltered, routing.FilteredDrop)
	}
	if p.DispatchMode != routing.DispatchSingle {
		t.Fatalf("DispatchMode: got %s, want %s", p.DispatchMode, routing.DispatchSingle)
	}
	if p.AckAfter != routing.AckAfterTargetAccept {
		t.Fatalf("AckAfter: got %s, want %s", p.AckAfter, routing.AckAfterTargetAccept)
	}
}

// TestRoutePolicy_WithDefaults_PreservesExplicit verifies WithDefaults does not overwrite explicitly set RoutePolicy and BackoffPolicy fields.
func TestRoutePolicy_WithDefaults_PreservesExplicit(t *testing.T) {
	p := routing.RoutePolicy{
		MaxInFlight:        50,
		MaxReplayAttempts:  10,
		ReplayBudget:       42 * time.Minute,
		OnExpired:          routing.ExpiredDrop,
		OnPermanentFailure: routing.FailureDrop,
		OnFiltered:         routing.FilteredDLQ,
		DispatchMode:       routing.DispatchFanOut,
		AckAfter:           routing.AckAfterOutboxPersist,
		Backoff: routing.BackoffPolicy{
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     10 * time.Second,
			Multiplier:      1.5,
		},
	}.WithDefaults()

	if p.MaxInFlight != 50 {
		t.Fatalf("explicit MaxInFlight should be preserved, got %d", p.MaxInFlight)
	}
	if p.MaxReplayAttempts != 10 {
		t.Fatalf("explicit MaxReplayAttempts should be preserved, got %d", p.MaxReplayAttempts)
	}
	if p.ReplayBudget != 42*time.Minute {
		t.Fatalf("explicit ReplayBudget should be preserved, got %v", p.ReplayBudget)
	}
	if p.OnExpired != routing.ExpiredDrop {
		t.Fatal("explicit OnExpired should be preserved")
	}
	if p.OnPermanentFailure != routing.FailureDrop {
		t.Fatal("explicit OnPermanentFailure should be preserved")
	}
	if p.OnFiltered != routing.FilteredDLQ {
		t.Fatal("explicit OnFiltered should be preserved")
	}
	if p.DispatchMode != routing.DispatchFanOut {
		t.Fatal("explicit DispatchMode should be preserved")
	}
	if p.AckAfter != routing.AckAfterOutboxPersist {
		t.Fatal("explicit AckAfter should be preserved")
	}
	if p.Backoff.InitialInterval != 500*time.Millisecond {
		t.Fatal("explicit Backoff.InitialInterval should be preserved")
	}
	if p.Backoff.Multiplier != 1.5 {
		t.Fatal("explicit Backoff.Multiplier should be preserved")
	}
}

// TestRoutePolicy_WithDefaults_SharedOutboxAckBoundary verifies that an unset
// AckAfter on a shared_outbox route defaults to outbox_persist — the boundary
// the delivery actually honors — rather than the global target_accept default.
func TestRoutePolicy_WithDefaults_SharedOutboxAckBoundary(t *testing.T) {
	p := routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox}.WithDefaults()
	if p.AckAfter != routing.AckAfterOutboxPersist {
		t.Fatalf("shared_outbox AckAfter default: got %s, want %s", p.AckAfter, routing.AckAfterOutboxPersist)
	}
}

// TestNewDefaultBackoffPolicy_Values validates the recommended default backoff values.
func TestNewDefaultBackoffPolicy_Values(t *testing.T) {
	bp := routing.NewDefaultBackoffPolicy()
	if bp.InitialInterval != 1*time.Second {
		t.Fatalf("InitialInterval: got %v, want 1s", bp.InitialInterval)
	}
	if bp.MaxInterval != 30*time.Second {
		t.Fatalf("MaxInterval: got %v, want 30s", bp.MaxInterval)
	}
	if bp.Multiplier != 2.0 {
		t.Fatalf("Multiplier: got %v, want 2.0", bp.Multiplier)
	}
}

// TestDeliveryMode_Constants validates that DeliveryMode enum values are distinct and non-empty.
func TestDeliveryMode_Constants(t *testing.T) {
	modes := []routing.DeliveryMode{
		routing.DeliveryDirectHold,
		routing.DeliverySharedOutbox,
	}
	seen := make(map[routing.DeliveryMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("DeliveryMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate DeliveryMode: %q", m)
		}
		seen[m] = true
	}
}

// TestDispatchMode_Constants validates that DispatchMode enum values are distinct and non-empty.
func TestDispatchMode_Constants(t *testing.T) {
	modes := []routing.DispatchMode{
		routing.DispatchSingle,
		routing.DispatchFanOut,
	}
	seen := make(map[routing.DispatchMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("DispatchMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate DispatchMode: %q", m)
		}
		seen[m] = true
	}
}

// TestSessionMode_Constants validates that SessionMode enum values are distinct and non-empty.
func TestSessionMode_Constants(t *testing.T) {
	modes := []connectivity.SessionMode{
		connectivity.SessionEphemeral,
		connectivity.SessionPersistent,
		connectivity.SessionExclusive,
	}
	seen := make(map[connectivity.SessionMode]bool, len(modes))
	for _, m := range modes {
		if m == "" {
			t.Fatal("SessionMode constant must not be empty")
		}
		if seen[m] {
			t.Fatalf("duplicate SessionMode: %q", m)
		}
		seen[m] = true
	}
}

// TestAckBoundary_Constants validates that AckBoundary enum values are distinct and non-empty.
func TestAckBoundary_Constants(t *testing.T) {
	values := []routing.AckBoundary{
		routing.AckAfterTargetAccept,
		routing.AckAfterOutboxPersist,
	}
	seen := make(map[routing.AckBoundary]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("AckBoundary constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate AckBoundary: %q", v)
		}
		seen[v] = true
	}
}

// TestExpiredAction_Constants validates that ExpiredAction enum values are distinct and non-empty.
func TestExpiredAction_Constants(t *testing.T) {
	values := []routing.ExpiredAction{
		routing.ExpiredDrop,
		routing.ExpiredDLQ,
	}
	seen := make(map[routing.ExpiredAction]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("ExpiredAction constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate ExpiredAction: %q", v)
		}
		seen[v] = true
	}
}

// TestFailureAction_Constants validates that FailureAction enum values are distinct and non-empty.
func TestFailureAction_Constants(t *testing.T) {
	values := []routing.FailureAction{
		routing.FailureDLQ,
		routing.FailureDrop,
	}
	seen := make(map[routing.FailureAction]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("FailureAction constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate FailureAction: %q", v)
		}
		seen[v] = true
	}
}

// TestFilteredAction_Constants validates that FilteredAction enum values are distinct and non-empty.
func TestFilteredAction_Constants(t *testing.T) {
	values := []routing.FilteredAction{
		routing.FilteredDrop,
		routing.FilteredDLQ,
	}
	seen := make(map[routing.FilteredAction]bool, len(values))
	for _, v := range values {
		if v == "" {
			t.Fatal("FilteredAction constant must not be empty")
		}
		if seen[v] {
			t.Fatalf("duplicate FilteredAction: %q", v)
		}
		seen[v] = true
	}
}

// TestFilteredAction_IsValid pins the enum's IsValid contract: only the two
// declared values are valid; the empty value and any typo are not.
func TestFilteredAction_IsValid(t *testing.T) {
	tests := []struct {
		value routing.FilteredAction
		valid bool
	}{
		{routing.FilteredDrop, true},
		{routing.FilteredDLQ, true},
		{"", false},
		{"DLQ", false},
		{"wat", false},
	}
	for _, tt := range tests {
		if got := tt.value.IsValid(); got != tt.valid {
			t.Fatalf("FilteredAction(%q).IsValid() = %v, want %v", tt.value, got, tt.valid)
		}
	}
}

// TestRoutePolicy_WithDefaults_FilteredDefaultsDrop is the domain half of F1:
// an unset OnFiltered defaults to drop, NOT the DLQ. This is the default that
// decouples a filter-drop from the permanent-failure DLQ sink.
func TestRoutePolicy_WithDefaults_FilteredDefaultsDrop(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()
	if p.OnFiltered != routing.FilteredDrop {
		t.Fatalf("OnFiltered default = %q, want %q", p.OnFiltered, routing.FilteredDrop)
	}
	// An unrecognised value is reset to the default by WithDefaults.
	p = routing.RoutePolicy{OnFiltered: "bogus"}.WithDefaults()
	if p.OnFiltered != routing.FilteredDrop {
		t.Fatalf("OnFiltered invalid value reset = %q, want %q", p.OnFiltered, routing.FilteredDrop)
	}
}

// TestRoutePolicy_Validate_OnFiltered pins the strict-validation gate: an empty
// OnFiltered validates (means default), a declared value validates, and a typo
// is rejected as a permanent InvalidConfig error — mirroring OnPermanentFailure.
func TestRoutePolicy_Validate_OnFiltered(t *testing.T) {
	tests := []struct {
		name    string
		value   routing.FilteredAction
		wantErr bool
	}{
		{name: "empty allowed (use default)", value: "", wantErr: false},
		{name: "drop allowed", value: routing.FilteredDrop, wantErr: false},
		{name: "dlq allowed", value: routing.FilteredDLQ, wantErr: false},
		{name: "typo rejected", value: "queue", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := routing.RoutePolicy{OnFiltered: tt.value}.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error for an invalid OnFiltered")
			}
			var be *shared.BridgeError
			if !errors.As(err, &be) {
				t.Fatalf("want *shared.BridgeError, got %T", err)
			}
			if be.Code != shared.ErrCodeInvalidConfig {
				t.Fatalf("error code = %v, want %v", be.Code, shared.ErrCodeInvalidConfig)
			}
			if be.Class != shared.ErrorPermanent {
				t.Fatalf("error class = %v, want %v", be.Class, shared.ErrorPermanent)
			}
		})
	}
}

// TestRoutePolicy_WithDefaults_NegativeValues verifies that negative values for
// MaxInFlight, MaxReplayAttempts, and MaxOutboxDepth are clamped to defaults.
func TestRoutePolicy_WithDefaults_NegativeValues(t *testing.T) {
	tests := []struct {
		name  string
		build func() routing.RoutePolicy
		check func(t *testing.T, p routing.RoutePolicy)
	}{
		{
			name: "negative MaxInFlight clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxInFlight: -1}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxInFlight != routing.DefaultMaxInFlight {
					t.Fatalf("MaxInFlight = %d, want %d (negative clamped)", p.MaxInFlight, routing.DefaultMaxInFlight)
				}
			},
		},
		{
			name: "negative MaxReplayAttempts clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxReplayAttempts: -5}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
					t.Fatalf("MaxReplayAttempts = %d, want %d (negative clamped)", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
				}
			},
		},
		{
			name: "negative ReplayBudget clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{ReplayBudget: -1 * time.Minute}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.ReplayBudget != routing.DefaultReplayBudget {
					t.Fatalf("ReplayBudget = %v, want %v (negative clamped)", p.ReplayBudget, routing.DefaultReplayBudget)
				}
			},
		},
		{
			name: "negative MaxOutboxDepth clamped to default",
			build: func() routing.RoutePolicy {
				return routing.RoutePolicy{MaxOutboxDepth: -100}.WithDefaults()
			},
			check: func(t *testing.T, p routing.RoutePolicy) {
				if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
					t.Fatalf("MaxOutboxDepth = %d, want %d (negative clamped)", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, tt.build())
		})
	}
}

// TestRoutePolicy_Validate_ReplayBudget verifies the strict-validation contract:
// a NEGATIVE ReplayBudget is a permanent BridgeError, while zero (means default)
// and any positive value validate cleanly. WithDefaults leniently clamps a
// negative to the default; Validate is the strict gate that rejects it.
func TestRoutePolicy_Validate_ReplayBudget(t *testing.T) {
	tests := []struct {
		name    string
		budget  time.Duration
		wantErr bool
	}{
		{name: "negative rejected", budget: -1 * time.Second, wantErr: true},
		{name: "zero allowed (use default)", budget: 0, wantErr: false},
		{name: "positive allowed", budget: 10 * time.Minute, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := routing.RoutePolicy{ReplayBudget: tt.budget}.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error for a negative ReplayBudget")
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

// TestRoutePolicy_WithDefaults_AllDefaults verifies that a completely zero-valued
// RoutePolicy receives every default, including SendTimeout and DepthCacheTTL.
func TestRoutePolicy_WithDefaults_AllDefaults(t *testing.T) {
	p := routing.RoutePolicy{}.WithDefaults()

	if p.MaxInFlight != routing.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want %d", p.MaxInFlight, routing.DefaultMaxInFlight)
	}
	if p.MaxReplayAttempts != routing.DefaultMaxReplayAttempts {
		t.Fatalf("MaxReplayAttempts = %d, want %d", p.MaxReplayAttempts, routing.DefaultMaxReplayAttempts)
	}
	if p.MaxOutboxDepth != routing.DefaultMaxOutboxDepth {
		t.Fatalf("MaxOutboxDepth = %d, want %d", p.MaxOutboxDepth, routing.DefaultMaxOutboxDepth)
	}
	if p.Backoff.InitialInterval != routing.NewDefaultBackoffPolicy().InitialInterval {
		t.Fatalf("Backoff.InitialInterval = %v, want %v", p.Backoff.InitialInterval, routing.NewDefaultBackoffPolicy().InitialInterval)
	}
	if p.Backoff.MaxInterval != routing.NewDefaultBackoffPolicy().MaxInterval {
		t.Fatalf("Backoff.MaxInterval = %v, want %v", p.Backoff.MaxInterval, routing.NewDefaultBackoffPolicy().MaxInterval)
	}
	if p.Backoff.Multiplier != routing.NewDefaultBackoffPolicy().Multiplier {
		t.Fatalf("Backoff.Multiplier = %v, want %v", p.Backoff.Multiplier, routing.NewDefaultBackoffPolicy().Multiplier)
	}
	if p.OnExpired != routing.ExpiredDLQ {
		t.Fatalf("OnExpired = %s, want %s", p.OnExpired, routing.ExpiredDLQ)
	}
	if p.OnPermanentFailure != routing.FailureDLQ {
		t.Fatalf("OnPermanentFailure = %s, want %s", p.OnPermanentFailure, routing.FailureDLQ)
	}
	if p.DispatchMode != routing.DispatchSingle {
		t.Fatalf("DispatchMode = %s, want %s", p.DispatchMode, routing.DispatchSingle)
	}
	if p.DeliveryMode != routing.DeliveryDirectHold {
		t.Fatalf("DeliveryMode = %s, want %s", p.DeliveryMode, routing.DeliveryDirectHold)
	}
	if p.AckAfter != routing.AckAfterTargetAccept {
		t.Fatalf("AckAfter = %s, want %s", p.AckAfter, routing.AckAfterTargetAccept)
	}
	if p.SendTimeout != routing.DefaultSendTimeout {
		t.Fatalf("SendTimeout = %v, want %v", p.SendTimeout, routing.DefaultSendTimeout)
	}
	if p.DepthCacheTTL != routing.DefaultDepthCacheTTL {
		t.Fatalf("DepthCacheTTL = %v, want %v", p.DepthCacheTTL, routing.DefaultDepthCacheTTL)
	}
}

// TestNewDefaultBackoffPolicy_Immutable verifies that NewDefaultBackoffPolicy
// returns an independent value: mutating one return value does not affect
// later calls.
func TestNewDefaultBackoffPolicy_Immutable(t *testing.T) {
	first := routing.NewDefaultBackoffPolicy()
	first.Multiplier = 99.0
	first.InitialInterval = 999 * time.Millisecond
	first.MaxInterval = 999 * time.Millisecond

	p := routing.RoutePolicy{}.WithDefaults()
	defaults := routing.NewDefaultBackoffPolicy()
	if p.Backoff.InitialInterval != defaults.InitialInterval {
		t.Fatalf("expected immutable InitialInterval %v, got %v", defaults.InitialInterval, p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval != defaults.MaxInterval {
		t.Fatalf("expected immutable MaxInterval %v, got %v", defaults.MaxInterval, p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier != defaults.Multiplier {
		t.Fatalf("expected immutable Multiplier %v, got %v", defaults.Multiplier, p.Backoff.Multiplier)
	}
}
