package validate_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// The blueprint validator is the LAST gate before a config transaction writes
// durably. A retry policy or session timing the builder rejects later — at
// apply or at the next restart — has already been committed by then and takes
// the rollback/divergence path. Every rule the builder enforces must therefore
// also hold here.

// sessionRouteConfig extends the shared valid-route fixture with an exclusive
// session block so the session-duration rules have something to check.
func sessionRouteConfig() *ports.BridgeConfig {
	cfg := validRouteWithResolver()
	cfg.Sessions = []ports.SessionDef{{ID: "s1", Transport: "mqtt", SessionMode: "exclusive"}}
	cfg.Senders[0].SessionID = "s1"
	cfg.Routes[0].Session = &ports.RouteSessionDef{SessionID: "s1", SenderID: "tx1"}
	return cfg
}

func jitterPtr(v float64) *float64 { return &v }

func TestValidateBlueprintGraph_SessionRouteFixture_Valid(t *testing.T) {
	if got := errorString(t, sessionRouteConfig()); got != "" {
		t.Fatalf("the session-route fixture must validate clean, got: %s", got)
	}
}

// TestValidateBlueprintGraph_NegativeBackoffIntervals rejects a negative retry
// interval before commit. time.ParseDuration happily accepts a leading '-', and
// a negative max_interval is the dangerous one: the exponential clamp is gated
// on `MaxInterval > 0`, so a negative cap never fires and the delay grows to
// +Inf.
func TestValidateBlueprintGraph_NegativeBackoffIntervals(t *testing.T) {
	for field, mutate := range map[string]func(*ports.BridgeConfig){
		"backoff.initial_interval": func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.InitialInterval = "-1s" },
		"backoff.max_interval":     func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.MaxInterval = "-30s" },
	} {
		t.Run(field, func(t *testing.T) {
			cfg := validRouteWithResolver()
			mutate(cfg)
			got := errorString(t, cfg)
			if !strings.Contains(got, field) {
				t.Fatalf("a negative %s must fail validation before commit, got: %q", field, got)
			}
		})
	}
}

// TestValidateBlueprintGraph_BackoffMultiplierBelowOne rejects decaying
// backoff: a multiplier in (0,1) makes each retry fire SOONER than the last.
func TestValidateBlueprintGraph_BackoffMultiplierBelowOne(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Policy.Backoff.Multiplier = 0.5
	got := errorString(t, cfg)
	if !strings.Contains(got, "backoff.multiplier") {
		t.Fatalf("a multiplier below 1 accelerates retries and must be rejected, got: %q", got)
	}
}

// TestValidateBlueprintGraph_BackoffMultiplierOne accepts the fixed-interval
// retry an operator gets with multiplier 1 — the new rule is `>= 1`, not `> 1`.
func TestValidateBlueprintGraph_BackoffMultiplierOne(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Policy.Backoff.Multiplier = 1.0
	if got := errorString(t, cfg); got != "" {
		t.Fatalf("multiplier 1.0 is a legal fixed retry interval, got: %s", got)
	}
}

// TestValidateBlueprintGraph_BackoffJitterBounds keeps the fraction in [0,1].
// An explicit 0 is legal — it is how an operator opts out of jitter — and is
// distinct from omitting the field, which takes the recommended default.
func TestValidateBlueprintGraph_BackoffJitterBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		jitter  *float64
		wantErr bool
	}{
		{name: "omitted", jitter: nil, wantErr: false},
		{name: "explicit zero opts out", jitter: jitterPtr(0), wantErr: false},
		{name: "fraction", jitter: jitterPtr(0.5), wantErr: false},
		{name: "one", jitter: jitterPtr(1), wantErr: false},
		{name: "above one", jitter: jitterPtr(1.5), wantErr: true},
		{name: "negative", jitter: jitterPtr(-0.1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validRouteWithResolver()
			cfg.Routes[0].Policy.Backoff.Jitter = tc.jitter
			got := errorString(t, cfg)
			if tc.wantErr && !strings.Contains(got, "backoff.jitter") {
				t.Fatalf("jitter must be a fraction in [0,1], got: %q", got)
			}
			if !tc.wantErr && got != "" {
				t.Fatalf("expected no errors, got: %s", got)
			}
		})
	}
}

// TestValidateBlueprintGraph_BrokerHealthStepDown covers the session duration
// field the builder parses but validation skipped, so an invalid value passed
// the config transaction and only failed at apply — after the durable write.
func TestValidateBlueprintGraph_BrokerHealthStepDown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "unparseable", value: "soon", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-30s", wantErr: true},
		{name: "positive", value: "45s", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sessionRouteConfig()
			cfg.Routes[0].Session.BrokerHealthStepDown = tc.value
			got := errorString(t, cfg)
			if tc.wantErr && !strings.Contains(got, "broker_health_step_down") {
				t.Fatalf("broker_health_step_down %q must fail before commit, got: %q", tc.value, got)
			}
			if !tc.wantErr && got != "" {
				t.Fatalf("expected no errors, got: %s", got)
			}
		})
	}
}
