package validate_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// The blueprint validator is the LAST gate before a config transaction writes
// durably. A lease cadence the session manager would only reach by clamping —
// or one no lease store can serve — must therefore be rejected HERE, not at
// apply: the admin transaction validates, writes, and only then applies, so a
// builder-only rule costs a durable write plus a failed apply plus a rollback.

// TestValidateBlueprintGraph_CollapsedLeaseCadenceRejected walks the profiles
// that leave no per-attempt renew budget. The derived renew interval and the
// standby acquire poll collapse toward the 1 ms floor: the owner renews back to
// back and every standby claims per millisecond, the store throttles, and those
// throttling errors are counted as transient renew failures — a self-inflicted
// overload that ends in an ownership change.
func TestValidateBlueprintGraph_CollapsedLeaseCadenceRejected(t *testing.T) {
	for name, mutate := range map[string]func(*ports.RouteSessionDef){
		"production_minimum_ttl": func(s *ports.RouteSessionDef) { s.LeaseTTL, s.MaxRenewFails = "5s", 5 },
		"ha_ttl":                 func(s *ports.RouteSessionDef) { s.LeaseTTL, s.MaxRenewFails = "45s", 50 },
		// Resolves to a 350 ms renew and a 350 ms poll — ABOVE the cadence floor,
		// so only the clamp rule catches it.
		"clamped_above_the_floor": func(s *ports.RouteSessionDef) { s.LeaseTTL, s.MaxRenewFails = "6s", 4 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := sessionRouteConfig()
			mutate(cfg.Routes[0].Session)

			got := errorString(t, cfg)
			if !strings.Contains(got, "leaves no room for the renew cadence") {
				t.Fatalf("expected the collapsed cadence to be rejected before the durable write, got: %s", got)
			}
		})
	}
}

// TestValidateBlueprintGraph_SubFloorLeaseCadenceRejected pins the two floor
// branches. Each case keeps the other resolved cadence healthy, so neither can
// be pinned by the other and neither is reachable through the clamp rule.
func TestValidateBlueprintGraph_SubFloorLeaseCadenceRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*ports.RouteSessionDef)
		want   string
	}{
		"renew_interval": {
			mutate: func(s *ports.RouteSessionDef) {
				s.LeaseTTL, s.RenewInterval, s.AcquirePollInterval = "360s", "100ms", "5s"
			},
			want: "resolved RenewInterval=100ms is below the minimum lease cadence",
		},
		"acquire_poll_interval": {
			mutate: func(s *ports.RouteSessionDef) {
				s.LeaseTTL, s.RenewInterval, s.RenewCallTimeout = "45s", "10s", "3s"
				s.AcquirePollInterval = "10ms"
			},
			want: "resolved AcquirePollInterval=10ms is below the minimum lease cadence",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := sessionRouteConfig()
			tc.mutate(cfg.Routes[0].Session)

			if got := errorString(t, cfg); !strings.Contains(got, tc.want) {
				t.Fatalf("expected %q, got: %s", tc.want, got)
			}
		})
	}
}

// TestValidateBlueprintGraph_HealthyLeaseCadenceAccepted is the negative
// control. It matters more than usual here: this rule runs on EVERY route
// session block, including the overwhelmingly common one that pins nothing, so
// a rule that over-rejects would fail working deployments at commit. The
// jitter-only clamp is included deliberately — the clamp sheds jitter first by
// design, and what remains is a healthy cadence.
func TestValidateBlueprintGraph_HealthyLeaseCadenceAccepted(t *testing.T) {
	for name, mutate := range map[string]func(*ports.RouteSessionDef){
		"nothing_pinned":       func(*ports.RouteSessionDef) {},
		"production_floor_ttl": func(s *ports.RouteSessionDef) { s.LeaseTTL = "5s" },
		"ha_shaped": func(s *ports.RouteSessionDef) {
			s.LeaseTTL, s.RenewInterval, s.RenewJitter, s.RenewCallTimeout, s.MaxRenewFails = "45s", "10s", "1s", "3s", 3
		},
		"jitter_only_clamp": func(s *ports.RouteSessionDef) { s.LeaseTTL, s.RenewJitter = "45s", "14s" },
		"poll_on_the_floor": func(s *ports.RouteSessionDef) { s.AcquirePollInterval = "250ms" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := sessionRouteConfig()
			mutate(cfg.Routes[0].Session)

			if got := errorString(t, cfg); got != "" {
				t.Fatalf("a healthy lease cadence must validate clean, got: %s", got)
			}
		})
	}
}

// TestValidateBlueprintGraph_UnparseableLeaseDurationReportedOnce guards against
// a confusing double diagnostic: an unparseable duration is already named by the
// field-level check, so the cadence rule must stay silent rather than resolve a
// partial request and blame a value the operator never wrote.
func TestValidateBlueprintGraph_UnparseableLeaseDurationReportedOnce(t *testing.T) {
	cfg := sessionRouteConfig()
	cfg.Routes[0].Session.LeaseTTL = "not-a-duration"

	got := errorString(t, cfg)
	if !strings.Contains(got, "invalid duration") {
		t.Fatalf("expected the field-level duration error, got: %s", got)
	}
	if strings.Contains(got, "lease cadence") {
		t.Fatalf("the cadence rule must not add a second error for an unparseable duration, got: %s", got)
	}
}
