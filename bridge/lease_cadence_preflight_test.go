package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func leaseCadenceBlueprint(sess *ports.RouteSessionDef) *ports.BridgeConfig {
	cfg := failoverTimingPluginConfig{timing: ports.TransportFailoverTiming{PostTakeoverActivation: 5 * time.Second}}
	sess.SessionID = "exclusive"
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "lease-cadence", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{{ID: "exclusive", Transport: cfg.Kind(), SessionMode: string(connectivity.SessionExclusive), Config: cfg}},
		Routes:   []ports.RouteDef{{ID: "route", Session: sess}},
	}
}

// TestBuilderPlan_RejectsCollapsedLeaseCadence pins that a lease cadence which
// would collapse to a millisecond store storm is refused by the composition
// root's preflight, before anything is built or durably committed.
//
// lease_ttl at the production minimum with max_renew_fails: 5 leaves no
// per-attempt budget: construction clamps the derived renew interval and the
// standby acquire poll to 1 ms and only WARNS, so the deployment starts and the
// lease store is what fails — and its throttling errors are counted as transient
// renew failures, turning a self-inflicted overload into an ownership change.
func TestBuilderPlan_RejectsCollapsedLeaseCadence(t *testing.T) {
	cfg := leaseCadenceBlueprint(&ports.RouteSessionDef{
		LeaseTTL:      "5s",
		MaxRenewFails: 5,
		StepDownGrace: "1s",
	})

	plan, err := NewBuilder(cfg).Plan(t.Context())
	if plan != nil {
		plan.Close()
	}
	require.Error(t, err, "a cadence the manager would silently clamp must not reach a built runtime")
	assert.True(t, errors.Is(err, shared.ErrInvalidConfig), "got %v", err)
	require.Nil(t, plan)
}

// TestBuilderPlan_AcceptsDerivedLeaseCadence is the negative control: the same
// blueprint with a tolerable failure count derives a cadence well above the
// floor and still plans, so the new preflight rejects only collapsed timings.
func TestBuilderPlan_AcceptsDerivedLeaseCadence(t *testing.T) {
	cfg := leaseCadenceBlueprint(&ports.RouteSessionDef{
		LeaseTTL:      "45s",
		MaxRenewFails: 3,
		StepDownGrace: "5s",
	})

	plan, err := NewBuilder(cfg).Plan(t.Context())
	require.NoError(t, err)
	require.NotNil(t, plan)
	plan.Close()
}
