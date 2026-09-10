package config

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestValidateConnectLeaseBudget_IncludesReconcile proves the failover-budget
// math now counts the reconcile phase, not connect alone. The chosen budget is
// UNDER the lease when only connect+first-renew is counted (the pre-fix formula)
// but OVER the lease once the reconcile budget (≈ a second connect) is folded
// in — so the advisory MUST fire, demonstrating reconcile is accounted for.
//
// connect=20s, renew=8s (jitter/2≈1s, renew_call_timeout≈4s), lease_ttl=45s:
//   - old span:  20 + 8 + 1 + 4        = 33s  (< 45 → would NOT warn)
//   - new span:  20 + 20 + 8 + 1 + 4   = 53s  (>= 45 → warns)
func TestValidateConnectLeaseBudget_IncludesReconcile(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.LeaseTTL = "45s"
	cfg.Routes[0].Session.RenewInterval = "8s"
	cfg.Sessions[0].SetDecoded(nil, fakeRawConfig(map[string]any{
		"connect_timeout": "20s",
	}))

	warnings, err := ValidateWithWarnings(cfg)
	require.NoError(t, err)
	require.True(t, hasConnectLeaseWarning(warnings),
		"reconcile budget must be counted: connect(20)+reconcile(20)+first-renew(~13) >= lease(45); warnings: %v", warnings)
	joined := strings.Join(warnings, "\n")
	assert.Contains(t, joined, "reconcile", "advisory should name the reconcile term")
}

// TestValidate_BuildTimeConsumedFields covers the finding that fields parsed
// only at build time (bridge/convert.go) escaped validation and turned into
// restart-time apply failures. Each bad value must now be rejected at
// validate-time.
func TestValidate_BuildTimeConsumedFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *ports.BridgeConfig)
		wantSub string
	}{
		{
			name:    "invalid acquire_poll_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.AcquirePollInterval = "banana" },
			wantSub: "acquire_poll_interval",
		},
		{
			name:    "invalid renew_call_timeout",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.RenewCallTimeout = "5" },
			wantSub: "renew_call_timeout",
		},
		{
			name:    "invalid lease_renew_jitter",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.RenewJitter = "notaduration" },
			wantSub: "lease_renew_jitter",
		},
		{
			name:    "invalid drain_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.DrainInterval = "10" },
			wantSub: "drain_interval",
		},
		{
			name:    "negative replay_budget",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.ReplayBudget = "-1s" },
			wantSub: "replay_budget",
		},
		{
			name:    "invalid backoff initial_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.InitialInterval = "soon" },
			wantSub: "backoff.initial_interval",
		},
		{
			name:    "invalid backoff max_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.MaxInterval = "later" },
			wantSub: "backoff.max_interval",
		},
		{
			name:    "backoff jitter out of range",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.Jitter = ptrTo(1.5) },
			wantSub: "backoff.jitter",
		},
		{
			name:    "negative backoff initial_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.InitialInterval = "-1s" },
			wantSub: "backoff.initial_interval",
		},
		{
			name:    "negative backoff max_interval",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.MaxInterval = "-30s" },
			wantSub: "backoff.max_interval",
		},
		{
			name:    "backoff multiplier below one",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Policy.Backoff.Multiplier = 0.5 },
			wantSub: "backoff.multiplier",
		},
		{
			name:    "invalid broker_health_step_down",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.BrokerHealthStepDown = "soon" },
			wantSub: "broker_health_step_down",
		},
		{
			name:    "non-positive broker_health_step_down",
			mutate:  func(c *ports.BridgeConfig) { c.Routes[0].Session.BrokerHealthStepDown = "0s" },
			wantSub: "broker_health_step_down",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := s12ValidConfig()
			tc.mutate(cfg)
			err := Validate(cfg)
			require.Error(t, err, "bad build-time-consumed field must fail validation")
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestValidate_BuildTimeConsumedFields_ValidPass is the negative control:
// well-formed values for the same fields must pass — the new checks must not
// reject a config the builder would accept.
func TestValidate_BuildTimeConsumedFields_ValidPass(t *testing.T) {
	cfg := s12ValidConfig()
	s := cfg.Routes[0].Session
	s.AcquirePollInterval = "5s"
	s.RenewCallTimeout = "3s"
	s.RenewJitter = "2s"
	s.DrainInterval = "10s"
	cfg.Routes[0].Policy.ReplayBudget = "15m"
	cfg.Routes[0].Policy.Backoff.InitialInterval = "1s"
	cfg.Routes[0].Policy.Backoff.MaxInterval = "30s"
	cfg.Routes[0].Policy.Backoff.Multiplier = 1.0
	cfg.Routes[0].Policy.Backoff.Jitter = ptrTo(0.5)
	s.BrokerHealthStepDown = "45s"

	require.NoError(t, Validate(cfg))
}

// ptrTo is the tri-state helper for optional numeric blueprint fields, where a
// nil pointer ("omitted") and an explicit zero mean different things.
func ptrTo[T any](v T) *T { return &v }

// TestManager_AppliedVersionSurfaced covers the cluster-convergence finding:
// per-instance divergence must at least be OBSERVABLE. The manager stamps and
// surfaces the version of the last config it applied so operators can detect
// cross-instance version skew.
func TestManager_AppliedVersionSurfaced(t *testing.T) {
	base := minimalValidConfig("base-bridge")
	base.Version = 7

	mgr := NewManager(Layer{Name: "file", Loader: &stubLoader{cfg: base}})

	if _, ok := mgr.AppliedVersion(); ok {
		t.Fatal("no version should be applied before Load")
	}

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	v, ok := mgr.AppliedVersion()
	require.True(t, ok, "a version must be surfaced after a successful Load")
	assert.Equal(t, 7, v)
}
