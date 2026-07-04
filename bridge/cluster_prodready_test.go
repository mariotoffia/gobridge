package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Finding C8 (cluster chunk, MEDIUM): binding-only (Path-2) sessions must
// inherit the route's lease timings instead of a hard-coded DefaultConfig
// (which pinned a ~6-minute failover on an otherwise 45s-tuned cluster).
// ---------------------------------------------------------------------------

// TestBindingSessionConfig_InheritsRouteLeaseTimings asserts that a session
// registered only through a binding derives its lease timings from the
// route's own session block — the same source the route's primary session
// uses — with the SessionID swapped for the binding's.
func TestBindingSessionConfig_InheritsRouteLeaseTimings(t *testing.T) {
	b := NewBuilder(&ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}})
	routeDef := ports.RouteDef{
		ID: "r1",
		Session: &ports.RouteSessionDef{
			SessionID:     "primary-sess",
			LeaseTTL:      "45s",
			RenewInterval: "14s",
			RenewJitter:   "1s",
			MaxRenewFails: 3,
			StepDownGrace: "5s",
		},
	}

	sc, err := b.bindingSessionConfig(routeDef, "binding-sess")
	require.NoError(t, err)

	assert.Equal(t, "binding-sess", sc.SessionID,
		"binding session must keep its OWN session id, not the route's")
	assert.Equal(t, 45*time.Second, sc.LeaseTTL,
		"binding session must inherit the route's lease_ttl (finding C8): "+
			"a hard-coded DefaultConfig pins a ~6-minute failover on an "+
			"otherwise HA-tuned cluster")
	assert.Equal(t, 14*time.Second, sc.RenewInterval)
	assert.Equal(t, 1*time.Second, sc.RenewJitter)
	assert.Equal(t, 3, sc.MaxRenewFails)
	assert.Equal(t, 5*time.Second, sc.StepDownGrace)
}

// TestBindingSessionConfig_NoRouteSession_LeavesRenewIntervalDerived asserts
// the fallback (route without a session block): defaults apply but
// RenewInterval stays zero so the session manager derives it from LeaseTTL
// (contract C3) instead of keeping DefaultConfig's pinned 110s.
func TestBindingSessionConfig_NoRouteSession_LeavesRenewIntervalDerived(t *testing.T) {
	b := NewBuilder(&ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}})

	sc, err := b.bindingSessionConfig(ports.RouteDef{ID: "r1"}, "binding-sess")
	require.NoError(t, err)

	assert.Equal(t, "binding-sess", sc.SessionID)
	assert.Equal(t, time.Duration(0), sc.RenewInterval,
		"RenewInterval must stay zero (derive-from-TTL downstream, contract C3)")
	assert.Equal(t, 360*time.Second, sc.LeaseTTL)
}

// ---------------------------------------------------------------------------
// Finding C11 (cluster chunk, LOW): endpoint-resolution failure must not
// silently disable cluster forwarding for the process lifetime in a clustered
// deployment.
// ---------------------------------------------------------------------------

type staticEndpointResolver struct {
	endpoints map[string]string
	err       error
}

func (r *staticEndpointResolver) Resolve(context.Context, string) (map[string]string, error) {
	return r.endpoints, r.err
}

// TestBuilder_Prepare_ClusteredResolverFailure_FailsStartup asserts that in a
// clustered deployment a failed endpoint resolution is a startup error: with
// nil endpoints, peers could never forward exclusive-route traffic to this
// instance, and the degradation would be silent for the whole process
// lifetime (finding C11).
func TestBuilder_Prepare_ClusteredResolverFailure_FailsStartup(t *testing.T) {
	cfg := directHoldConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	_, err := buildWith(cfg, "sqs").
		RegisterEndpointResolver(&staticEndpointResolver{err: errors.New("metadata endpoint unreachable")}).
		prepare(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint resolution failed")
	assert.Contains(t, err.Error(), "metadata endpoint unreachable")
}

// TestBuilder_Prepare_NonClusteredResolverFailure_WarnsAndContinues asserts
// the single-instance posture is unchanged: a resolver failure outside
// clustered mode degrades to no cluster forwarding without failing startup.
func TestBuilder_Prepare_NonClusteredResolverFailure_WarnsAndContinues(t *testing.T) {
	cfg := directHoldConfig()

	_, err := buildWith(cfg, "sqs").
		RegisterEndpointResolver(&staticEndpointResolver{err: errors.New("metadata endpoint unreachable")}).
		prepare(context.Background())

	require.NoError(t, err)
}

// TestBuilder_Prepare_ClusteredResolverSuccess_UsesEndpoints asserts the
// happy path: a successful resolution in clustered mode passes prepare.
func TestBuilder_Prepare_ClusteredResolverSuccess_UsesEndpoints(t *testing.T) {
	cfg := directHoldConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	_, err := buildWith(cfg, "sqs").
		RegisterEndpointResolver(&staticEndpointResolver{
			endpoints: map[string]string{"http": "http://10.0.0.5:8080"},
		}).
		prepare(context.Background())

	require.NoError(t, err)
}
