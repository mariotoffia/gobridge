package bridge

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

func jitter(v float64) *float64 { return &v }

// TestToRoutePolicy_BackoffJitterOmitted_GetsRecommendedDefault is the
// regression: the recommended 0.2 equal-jitter reached only policies built from
// NewDefaultBackoffPolicy, so every route loaded from a blueprint retried with
// ZERO jitter and a whole replica set re-attempted a failed target on the same
// tick. Omitting `jitter` must land on the same default a programmatic policy
// gets.
func TestToRoutePolicy_BackoffJitterOmitted_GetsRecommendedDefault(t *testing.T) {
	p, err := toRoutePolicyE(ports.RouteDef{})
	if err != nil {
		t.Fatalf("toRoutePolicyE: %v", err)
	}
	got := p.WithDefaults().Backoff.JitterFactor
	if got != routing.DefaultJitterFactor {
		t.Fatalf("config-loaded route jitter = %v, want the recommended %v (the value a "+
			"programmatic NewDefaultBackoffPolicy route already gets)", got, routing.DefaultJitterFactor)
	}
}

// TestToRoutePolicy_BackoffJitterExplicitZero_StaysDeterministic proves the
// tri-state survives the conversion: `jitter: 0` is an operator opting OUT of
// jitter, not an omitted field, so defaulting must leave the retry deterministic.
func TestToRoutePolicy_BackoffJitterExplicitZero_StaysDeterministic(t *testing.T) {
	p, err := toRoutePolicyE(ports.RouteDef{
		Policy: ports.PolicyDef{Backoff: ports.BackoffDef{Jitter: jitter(0)}},
	})
	if err != nil {
		t.Fatalf("toRoutePolicyE: %v", err)
	}
	if jf := p.WithDefaults().Backoff.JitterFactor; jf > 0 {
		t.Fatalf("explicit jitter: 0 became %v; an explicit opt-out must stay deterministic", jf)
	}
}

// TestToRoutePolicy_BackoffBoundsRejected pins the builder to the same retry
// rules the blueprint validator enforces, so a route built directly through the
// library API cannot receive a policy the config path would refuse.
func TestToRoutePolicy_BackoffBoundsRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backoff ports.BackoffDef
		wantSub string
	}{
		{
			name:    "negative initial_interval",
			backoff: ports.BackoffDef{InitialInterval: "-1s"},
			wantSub: "initial_interval",
		},
		{
			name:    "negative max_interval",
			backoff: ports.BackoffDef{MaxInterval: "-30s"},
			wantSub: "max_interval",
		},
		{
			name:    "multiplier below one decays",
			backoff: ports.BackoffDef{Multiplier: 0.5},
			wantSub: "multiplier",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := toRoutePolicyE(ports.RouteDef{Policy: ports.PolicyDef{Backoff: tc.backoff}})
			if err == nil {
				t.Fatalf("%s must be rejected by the builder", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q must name %q", err, tc.wantSub)
			}
		})
	}
}

// TestToRoutePolicy_BackoffMultiplierOneAccepted is the negative control for
// the `>= 1` rule: a fixed retry interval is a legitimate configuration.
func TestToRoutePolicy_BackoffMultiplierOneAccepted(t *testing.T) {
	p, err := toRoutePolicyE(ports.RouteDef{
		Policy: ports.PolicyDef{Backoff: ports.BackoffDef{Multiplier: 1.0}},
	})
	if err != nil {
		t.Fatalf("multiplier 1.0 is a legal fixed interval: %v", err)
	}
	if p.Backoff.Multiplier != 1.0 {
		t.Fatalf("Backoff.Multiplier = %v, want 1.0", p.Backoff.Multiplier)
	}
}
