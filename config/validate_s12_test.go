package config

import (
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════
// S12 Validation Tests — staleClaimDuration and session durations
//
// Validates:
//   - Warning when stale_claim_duration > 2 * max(step_down_grace)
//   - No warning when within bounds or not explicitly set
//   - Error on invalid duration strings
//   - Error on unknown stale_claim_duration types
//   - Error on negative or zero session durations
// ═══════════════════════════════════════════════════════════════════════

func s12ValidConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "test-bridge"},
		Receivers: []ports.ReceiverDef{{ID: "rx1", Transport: "sqs"}},
		Senders:   []ports.SenderDef{{ID: "tx1", Transport: "mqtt", SessionID: "s1"}},
		Sessions:  []ports.SessionDef{{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx1", Address: "topic/a"}},
		Routes: []ports.RouteDef{{
			ID:           "r1",
			ReceiverID:   "rx1",
			DeliveryMode: "shared_outbox",
			Bindings:     []string{"b1"},
			Session:      &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"},
		}},
		Stores: ports.StoresConfig{
			Outbox: &ports.StoreConfig{Type: "dynamodb"},
			Lease:  &ports.StoreConfig{Type: "dynamodb"},
		},
	}
}

// TestValidateStaleClaimDuration_NoWarning_WithinBounds validates that
// a stale_claim_duration within 2x the step_down_grace produces no warning.
func TestValidateStaleClaimDuration_NoWarning_WithinBounds(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.StepDownGrace = "15s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "25s"}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "stale_claim_duration") {
			t.Errorf("unexpected stale_claim_duration warning: %s", w)
		}
	}
}

// TestValidateStaleClaimDuration_Warning_ExceedsTwiceGrace validates that
// a stale_claim_duration exceeding 2x the maximum step_down_grace emits
// a warning.
func TestValidateStaleClaimDuration_Warning_ExceedsTwiceGrace(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.StepDownGrace = "15s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "120s"}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, w := range warnings {
		if strings.Contains(w, "more than 2x") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about stale_claim_duration exceeding 2x grace, got warnings: %v", warnings)
	}
}

// TestValidateStaleClaimDuration_NoWarning_WhenNotExplicit validates that
// auto-derived stale_claim_duration (no explicit value) produces no warning.
func TestValidateStaleClaimDuration_NoWarning_WhenNotExplicit(t *testing.T) {
	cfg := s12ValidConfig()
	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "stale_claim_duration") {
			t.Errorf("unexpected warning: %s", w)
		}
	}
}

// TestValidateStaleClaimDuration_NoWarning_NoOutboxStore validates that
// missing stores.outbox does not cause a panic.
func TestValidateStaleClaimDuration_NoWarning_NoOutboxStore(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Stores.Outbox = nil

	_, err := ValidateWithWarnings(cfg)
	if err != nil {
		for _, e := range err.(*ValidationError).Errors {
			if strings.Contains(e, "stale_claim_duration") {
				t.Errorf("unexpected stale_claim_duration error: %s", e)
			}
		}
	}
}

// TestValidateStaleClaimDuration_TimeDurationType validates that
// time.Duration values (from builder injection) are handled correctly.
func TestValidateStaleClaimDuration_TimeDurationType(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.StepDownGrace = "15s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{
		"stale_claim_duration": 120 * time.Second,
	}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "more than 2x") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning for time.Duration value exceeding 2x grace, got warnings: %v", warnings)
	}
}

// TestValidateStaleClaimDuration_InvalidDurationString validates that
// an unparseable duration string produces a validation error.
func TestValidateStaleClaimDuration_InvalidDurationString(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{
		"stale_claim_duration": "not-a-duration",
	}))

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for invalid duration")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "invalid duration") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'invalid duration' error, got errors: %v", ve.Errors)
	}
}

// TestValidateStaleClaimDuration_UnknownType validates that an unsupported
// type (e.g. int) produces a validation error.
func TestValidateStaleClaimDuration_UnknownType(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{
		"stale_claim_duration": 30,
	}))

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for int type")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "must be a duration string") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected type error, got errors: %v", ve.Errors)
	}
}

// TestValidateStaleClaimDuration_DefaultGrace_WhenNoRoutesHaveGrace
// validates that the warning threshold uses the default 15s grace when
// no routes explicitly set step_down_grace.
func TestValidateStaleClaimDuration_DefaultGrace_WhenNoRoutesHaveGrace(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{
		"stale_claim_duration": "120s",
	}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "more than 2x") && strings.Contains(w, "15s") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning referencing default 15s grace, got warnings: %v", warnings)
	}
}

// TestValidateSessionDurations_NegativeLeaseTTL validates that a negative
// lease_ttl produces a validation error.
func TestValidateSessionDurations_NegativeLeaseTTL(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.LeaseTTL = "-5s"

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for negative lease_ttl")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "lease_ttl") && strings.Contains(e, "must be positive") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected positive-duration error for lease_ttl, got: %v", ve.Errors)
	}
}

// TestValidateSessionDurations_InvalidStepDownGrace validates that an
// unparseable step_down_grace string produces a validation error.
func TestValidateSessionDurations_InvalidStepDownGrace(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.StepDownGrace = "banana"

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for invalid step_down_grace")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "step_down_grace") && strings.Contains(e, "invalid duration") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-duration error for step_down_grace, got: %v", ve.Errors)
	}
}

// TestValidatePolicyDurations_InvalidSendTimeout validates that an
// unparseable send_timeout string produces a validation error.
func TestValidatePolicyDurations_InvalidSendTimeout(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "not-a-duration"

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for invalid send_timeout")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "send_timeout") && strings.Contains(e, "invalid duration") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-duration error for send_timeout, got: %v", ve.Errors)
	}
}

// TestValidatePolicyDurations_InvalidDepthCacheTTL validates that an
// unparseable depth_cache_ttl string produces a validation error.
func TestValidatePolicyDurations_InvalidDepthCacheTTL(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.DepthCacheTTL = "garbage"

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("expected validation error for invalid depth_cache_ttl")
	}
	ve := err.(*ValidationError)
	found := false
	for _, e := range ve.Errors {
		if strings.Contains(e, "depth_cache_ttl") && strings.Contains(e, "invalid duration") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-duration error for depth_cache_ttl, got: %v", ve.Errors)
	}
}

// TestValidatePolicyDurations_ValidSendTimeout validates that a valid
// send_timeout string passes validation without errors.
func TestValidatePolicyDurations_ValidSendTimeout(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "10s"

	_, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("expected no error for valid send_timeout, got: %v", err)
	}
}
