package config

import (
	"strings"
	"testing"
	"time"
)

// warnSubstrConnectLease is the distinctive fragment of the F2 advisory emitted
// by validateConnectLeaseBudget; tests key off it to avoid matching unrelated
// warnings (e.g. the direct_hold fencing advisory).
const warnSubstrConnectLease = "connect + first-renew span"

func boolPtrConfig(b bool) *bool { return &b }

// hasConnectLeaseWarning reports whether the F2 advisory is present.
func hasConnectLeaseWarning(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, warnSubstrConnectLease) {
			return true
		}
	}
	return false
}

// TestValidateConnectLeaseBudget covers finding F2: a deferred-connect
// (connect_after_lease) lease-bound session whose connect_timeout consumes so
// much of the lease TTL that the first renewal can complete at/after expiry must
// emit an advisory warning; eager-connect, unset-connect_timeout, and
// comfortably-budgeted sessions must not.
func TestValidateConnectLeaseBudget(t *testing.T) {
	tests := []struct {
		name           string
		leaseTTL       string
		renewInterval  string
		connectTimeout any   // value stored under the session's raw connect_timeout; nil means absent
		connectAfter   *bool // route session ConnectAfterLease (nil => default true)
		wantWarn       bool
	}{
		{
			// pinned renew fits the renew invariant (13s*3=39 < 45) yet
			// connect_timeout(35s)+first-renew(13s)=48 >= 45 => warn.
			name:           "tight_deferred_explicit_connect_warns",
			leaseTTL:       "45s",
			renewInterval:  "8s",
			connectTimeout: "35s",
			wantWarn:       true,
		},
		{
			// same tight budget but eager connect happens BEFORE acquire => skip.
			name:           "tight_but_eager_connect_no_warn",
			leaseTTL:       "45s",
			renewInterval:  "8s",
			connectTimeout: "35s",
			connectAfter:   boolPtrConfig(false),
			wantWarn:       false,
		},
		{
			// nil ConnectAfterLease defaults to true (F6) => still warns.
			name:           "tight_default_deferred_warns",
			leaseTTL:       "45s",
			renewInterval:  "8s",
			connectTimeout: "35s",
			connectAfter:   nil,
			wantWarn:       true,
		},
		{
			// ample TTL => 48 < 120 => no warn.
			name:           "ample_ttl_no_warn",
			leaseTTL:       "120s",
			renewInterval:  "8s",
			connectTimeout: "35s",
			wantWarn:       false,
		},
		{
			// connect_timeout not set => cannot bound the connect budget => skip.
			name:           "no_connect_timeout_no_warn",
			leaseTTL:       "45s",
			renewInterval:  "8s",
			connectTimeout: nil,
			wantWarn:       false,
		},
		{
			// derived renew_interval (empty) + large explicit connect_timeout:
			// 42 + ~9 = ~51 >= 45 => warn (exercises derivedRenewIntervalForConfig).
			name:           "derived_renew_large_connect_warns",
			leaseTTL:       "45s",
			renewInterval:  "",
			connectTimeout: "42s",
			wantWarn:       true,
		},
		{
			// a bare int (not a time.Duration) is NOT interpreted as a duration
			// by the transport-neutral peek => skipped, no warning.
			name:           "int_typed_connect_skipped",
			leaseTTL:       "45s",
			renewInterval:  "8s",
			connectTimeout: 35_000_000_000, // int, not time.Duration => default branch
			wantWarn:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := s12ValidConfig()
			cfg.Routes[0].Session.LeaseTTL = tc.leaseTTL
			cfg.Routes[0].Session.RenewInterval = tc.renewInterval
			cfg.Routes[0].Session.ConnectAfterLease = tc.connectAfter
			if tc.connectTimeout != nil {
				cfg.Sessions[0].SetDecoded(nil, fakeRawConfig(map[string]any{
					"connect_timeout": tc.connectTimeout,
				}))
			}

			warnings, err := ValidateWithWarnings(cfg)
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if got := hasConnectLeaseWarning(warnings); got != tc.wantWarn {
				t.Errorf("connect-lease warning = %v, want %v (warnings: %v)", got, tc.wantWarn, warnings)
			}
		})
	}
}

// TestValidateConnectLeaseBudget_DurationType confirms a time.Duration-typed
// connect_timeout in the raw map is honored (mirrors the stale_claim_duration
// duration-type path) and triggers the advisory when the budget is tight.
func TestValidateConnectLeaseBudget_DurationType(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session.LeaseTTL = "45s"
	cfg.Routes[0].Session.RenewInterval = "8s"
	cfg.Sessions[0].SetDecoded(nil, fakeRawConfig(map[string]any{
		"connect_timeout": 35 * time.Second,
	}))

	warnings, err := ValidateWithWarnings(cfg)
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if !hasConnectLeaseWarning(warnings) {
		t.Errorf("expected connect-lease warning for time.Duration connect_timeout, got: %v", warnings)
	}
}

// TestDerivedRenewIntervalForConfig_GoldenValues is a drift-guard for finding F2.
// derivedRenewIntervalForConfig is a hand-copy of runtime/session.deriveRenewInterval
// (the config package cannot import runtime/session without violating the layering
// rule), so the two can silently diverge and make the F2 advisory compute the
// wrong renew interval. A full cross-package equivalence test is impossible (both
// funcs are unexported in different modules), so instead this pins the derived
// output for representative inputs. The golden values below were computed from the
// shared formula; if this test fails, deriveRenewInterval and
// derivedRenewIntervalForConfig have drifted — reconcile them (update BOTH funcs
// and these goldens together).
func TestDerivedRenewIntervalForConfig_GoldenValues(t *testing.T) {
	cases := []struct {
		name          string
		ttl           time.Duration
		maxRenewFails int
		want          time.Duration
	}{
		// Default profile: LeaseTTL 360s, MaxRenewFails 3.
		{"default_360s_mf3", 360 * time.Second, 3, 75555555555},
		// MaxRenewFails <= 0 is floored to 1 (matches deriveRenewInterval).
		{"floored_mf0", 360 * time.Second, 0, 235555555555},
		// Tight single-attempt profile (the F1 split-brain-sensitive shape).
		{"tight_45s_mf1", 45 * time.Second, 1, 25555555555},
		{"small_30s_mf3", 30 * time.Second, 3, 3333333333},
		{"common_60s_mf3", 60 * time.Second, 3, 8888888888},
		// Tiny TTL exercises the reserve>=perAttempt/2 clamp and the 1ms floor.
		{"floor_1ms", 1 * time.Millisecond, 3, time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivedRenewIntervalForConfig(tc.ttl, tc.maxRenewFails); got != tc.want {
				t.Fatalf("derivedRenewIntervalForConfig(%s, %d) = %v (%d ns), want %v (%d ns) — "+
					"config/runtime renew-interval derivation has drifted",
					tc.ttl, tc.maxRenewFails, got, int64(got), tc.want, int64(tc.want))
			}
		})
	}
}

// TestValidateConnectLeaseBudget_NonLeaseRoute confirms routes without a lease
// session are ignored even if the session declares a connect_timeout.
func TestValidateConnectLeaseBudget_NonLeaseRoute(t *testing.T) {
	cfg := s12ValidConfig()
	cfg.Routes[0].Session = nil
	cfg.Routes[0].DeliveryMode = "direct"
	cfg.Sessions[0].SetDecoded(nil, fakeRawConfig(map[string]any{"connect_timeout": "35s"}))

	warnings, _ := ValidateWithWarnings(cfg)
	if hasConnectLeaseWarning(warnings) {
		t.Errorf("did not expect connect-lease warning for non-lease route, got: %v", warnings)
	}
}
