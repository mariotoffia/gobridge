package bridge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// TestBlueprintLeaseCadenceMatchesBuilder is the anti-drift contract behind the
// pre-commit cadence rule.
//
// The blueprint validator must reject a lease cadence BEFORE the admin config
// transaction's durable write, so it resolves the cadence itself from the raw
// route session block. The builder resolves the same cadence a second time on
// its way to a session.Config. Both call the domain's one resolver, but each
// applies the baseline a route session inherits on its own — the validator via
// routing.BaselineLeaseTiming, the builder via the HAConfig/DefaultConfig branch
// in toSessionConfigE. If those two ever disagree, the validator starts judging
// configurations by values the runtime will not use, which is exactly the class
// of defect the shared resolver was introduced to end.
//
// This test walks the shapes an operator actually writes, in both deployment
// modes, and asserts the two paths resolve to the identical cadence.
func TestBlueprintLeaseCadenceMatchesBuilder(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess ports.RouteSessionDef
	}{
		{name: "nothing_pinned"},
		{name: "ttl_only", sess: ports.RouteSessionDef{LeaseTTL: "45s"}},
		{name: "ttl_and_fails", sess: ports.RouteSessionDef{LeaseTTL: "45s", MaxRenewFails: 5}},
		{name: "renew_only", sess: ports.RouteSessionDef{RenewInterval: "9s"}},
		{name: "jitter_only", sess: ports.RouteSessionDef{RenewJitter: "2s"}},
		{name: "poll_only", sess: ports.RouteSessionDef{AcquirePollInterval: "3s"}},
		{name: "call_timeout_only", sess: ports.RouteSessionDef{RenewCallTimeout: "2s"}},
		{
			name: "fully_pinned",
			sess: ports.RouteSessionDef{
				LeaseTTL: "60s", RenewInterval: "12s", RenewJitter: "1s",
				RenewCallTimeout: "4s", AcquirePollInterval: "3s", MaxRenewFails: 3,
			},
		},
		{name: "collapsed", sess: ports.RouteSessionDef{LeaseTTL: "5s", MaxRenewFails: 5}},
		{name: "clamped_by_jitter", sess: ports.RouteSessionDef{LeaseTTL: "45s", RenewJitter: "14s"}},
	} {
		for _, clustered := range []bool{false, true} {
			name := tc.name
			if clustered {
				name += "/clustered"
			}
			t.Run(name, func(t *testing.T) {
				sess := tc.sess
				sess.SessionID = "s1"
				sess.SenderID = "tx1"

				sc, err := toSessionConfigE(&sess, clustered)
				require.NoError(t, err)
				fromBuilder := sc.EffectiveLeaseCadence()

				pinned := blueprintPinnedLeaseTiming(t, &sess)
				fromBlueprint := routing.BaselineLeaseTiming(clustered, pinned).ApplyOverrides(pinned).Resolve()

				require.Equal(t, fromBuilder, fromBlueprint,
					"the blueprint validator and the builder must resolve the same cadence, or preflight "+
						"judges a configuration by values the runtime will not run")
			})
		}
	}
}

// blueprintPinnedLeaseTiming mirrors validate.pinnedLeaseTiming, which is
// unexported. Keeping the mirror here (rather than exporting the validator's
// internals) is deliberate: this test proves the two RESOLUTIONS agree, and a
// parse helper that silently diverged would be caught by the same assertion.
func blueprintPinnedLeaseTiming(t *testing.T, sess *ports.RouteSessionDef) routing.LeaseTimingRequest {
	t.Helper()
	parse := func(v string) time.Duration {
		if v == "" {
			return 0
		}
		d, err := time.ParseDuration(v)
		require.NoError(t, err)
		return d
	}
	return routing.LeaseTimingRequest{
		LeaseTTL:            parse(sess.LeaseTTL),
		RenewInterval:       parse(sess.RenewInterval),
		RenewJitter:         parse(sess.RenewJitter),
		RenewCallTimeout:    parse(sess.RenewCallTimeout),
		AcquirePollInterval: parse(sess.AcquirePollInterval),
		MaxRenewFails:       sess.MaxRenewFails,
	}
}
