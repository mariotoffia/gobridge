package config

import (
	"strings"
	"testing"
)

// A wall-clock stale-claim reclaim is crash recovery: it hands a record to a
// second sender on the assumption the first one is dead. Set below the time a
// HEALTHY owner can hold a claim, it stops being recovery and starts duplicating
// deliveries that are still in flight — silently, because nothing fails. These
// pin the two bounds validation applies to an EXPLICIT value.

func TestValidateStaleClaimDuration_BelowSendTimeout_IsRejected(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "60s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "45s"}))

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("a stale_claim_duration below the route send_timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "send_timeout") {
		t.Fatalf("the error must name the send_timeout it violates, got: %v", err)
	}
}

// The bound is the LARGEST effective send timeout across routes: one slow route
// is enough to make a short reclaim window unsafe for the whole deployment.
func TestValidateStaleClaimDuration_UsesLargestRouteSendTimeout(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "5s"
	slow := cfg.Routes[0]
	slow.ID = "r2"
	slow.Policy.SendTimeout = "90s"
	cfg.Routes = append(cfg.Routes, slow)
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "60s"}))

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("the slowest route's send_timeout must set the floor for the whole deployment")
	}
}

// A route with no explicit send_timeout contributes the runtime default, so the
// floor cannot be dodged by omitting the field.
func TestValidateStaleClaimDuration_UnsetSendTimeoutUsesTheDefault(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "20s"}))

	_, err := ValidateWithWarnings(cfg)
	if err == nil {
		t.Fatal("a value below the DEFAULT send timeout must be rejected even with no explicit policy")
	}
}

// Above the send timeout but below the full batch ceiling the overlap depends on
// where a record sat in its batch: real, but configuration-dependent. That is a
// warning naming the window, not a rejection.
func TestValidateStaleClaimDuration_BelowBatchCeiling_Warns(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "10s"
	cfg.Bridge.MaxDrainTimeout = "30s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "20s"}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("a value above the send timeout must not be rejected: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "in-flight claim ceiling") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an in-flight-ceiling warning, got %v", warnings)
	}
}

// Clear of both bounds: no error, no in-flight warning.
func TestValidateStaleClaimDuration_AboveBatchCeiling_IsClean(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Policy.SendTimeout = "10s"
	cfg.Bridge.MaxDrainTimeout = "10s"
	cfg.Routes[0].Session.StepDownGrace = "30s"
	cfg.Stores.Outbox.SetDecoded(nil, fakeRawConfig(map[string]any{"stale_claim_duration": "40s"}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "stale_claim_duration") {
			t.Fatalf("unexpected stale_claim_duration warning: %s", w)
		}
	}
}
