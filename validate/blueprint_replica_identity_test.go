package validate_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replicaIdentityTestConfig struct{ strategy string }

type typedNilReplicaIdentityConfig struct{}

func (*typedNilReplicaIdentityConfig) Kind() string    { return "shared-consumer" }
func (*typedNilReplicaIdentityConfig) Validate() error { return nil }
func (*typedNilReplicaIdentityConfig) ReplicaIdentityStrategy() string {
	panic("typed nil replica identity invoked")
}

func (replicaIdentityTestConfig) Kind() string                      { return "shared-consumer" }
func (replicaIdentityTestConfig) Validate() error                   { return nil }
func (c replicaIdentityTestConfig) ReplicaIdentityStrategy() string { return c.strategy }

func clusteredSharedConfig(mode string, cfg ports.PluginConfig) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "bridge", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{{ID: "session", Transport: "custom", SessionMode: mode, Config: cfg}},
		Receivers: []ports.ReceiverDef{{
			ID: "receiver", SessionID: "session",
			Topics: []ports.SubscriptionDef{{Topic: "$share/workers/events/#"}},
		}},
	}
}

func TestValidateBlueprintGraph_ClusteredSharedRequiresVerifiableReplicaIdentity(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		cfg     ports.PluginConfig
		wantErr bool
	}{
		{name: "persistent hostname", mode: "persistent", cfg: replicaIdentityTestConfig{strategy: "hostname"}},
		{name: "ephemeral hostname", mode: "ephemeral", cfg: replicaIdentityTestConfig{strategy: "hostname"}},
		{name: "ephemeral nonce", mode: "ephemeral", cfg: replicaIdentityTestConfig{strategy: "nonce"}},
		{name: "persistent nonce", mode: "persistent", cfg: replicaIdentityTestConfig{strategy: "nonce"}, wantErr: true},
		{name: "capability unavailable", mode: "persistent", cfg: nil, wantErr: true},
		{name: "strategy empty", mode: "persistent", cfg: replicaIdentityTestConfig{}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := validate.ValidateBlueprintGraph(clusteredSharedConfig(tc.mode, tc.cfg))
			if tc.wantErr {
				require.NotNil(t, result)
				assert.True(t, result.HasErrors())
				assert.Contains(t, result.Error(), "replica identity")
				return
			}
			if result != nil {
				assert.False(t, result.HasErrors(), result.Error())
			}
		})
	}
}

func TestValidateBlueprintGraph_ClusteredExclusiveRejectsReplicaSuffix(t *testing.T) {
	result := validate.ValidateBlueprintGraph(clusteredSharedConfig("exclusive", replicaIdentityTestConfig{strategy: "hostname"}))
	require.NotNil(t, result)
	assert.True(t, result.HasErrors())
	assert.Contains(t, result.Error(), "exclusive")
	assert.Contains(t, result.Error(), "replica identity")
}

func TestValidateBlueprintGraph_StandaloneSharedDoesNotRequireReplicaCapability(t *testing.T) {
	cfg := clusteredSharedConfig("persistent", nil)
	cfg.Bridge.DeploymentMode = "standalone"
	result := validate.ValidateBlueprintGraph(cfg)
	if result != nil {
		assert.False(t, result.HasErrors(), result.Error())
	}
}

// TestValidateBlueprintGraph_StaticEndpointsTriggersClusteredValidation pins
// CLUSTER-1: a deployment that activates clustered runtime behavior via a static
// cluster.endpoints override (NOT deployment_mode: clustered) must run the SAME
// clustered replica-safety validation. Before the fix, only the deployment_mode
// spelling triggered it, so a static-endpoints cohort could pass validation while
// N-fold consuming the same traffic or colliding on ClientID.
func TestValidateBlueprintGraph_StaticEndpointsTriggersClusteredValidation(t *testing.T) {
	// Missing replica identity capability on a shared subscription must error under
	// BOTH clustered spellings identically.
	viaMode := clusteredSharedConfig("persistent", nil)
	viaEndpoints := clusteredSharedConfig("persistent", nil)
	viaEndpoints.Bridge.DeploymentMode = "" // not the explicit clustered spelling
	viaEndpoints.Bridge.Cluster = &ports.ClusterConfig{Endpoints: map[string]string{"http": "http://10.0.1.10:8080"}}

	modeResult := validate.ValidateBlueprintGraph(viaMode)
	endpointsResult := validate.ValidateBlueprintGraph(viaEndpoints)

	require.NotNil(t, modeResult)
	require.NotNil(t, endpointsResult, "static cluster.endpoints must trigger clustered validation (CLUSTER-1)")
	assert.True(t, endpointsResult.HasErrors())
	assert.Contains(t, endpointsResult.Error(), "replica identity")
}

func TestValidateBlueprintGraph_TypedNilReplicaIdentityReturnsError(t *testing.T) {
	var typedNil *typedNilReplicaIdentityConfig
	var result *ports.BlueprintValidationError
	require.NotPanics(t, func() {
		result = validate.ValidateBlueprintGraph(clusteredSharedConfig("persistent", typedNil))
	})
	require.NotNil(t, result)
	assert.True(t, result.HasErrors())
	assert.Contains(t, result.Error(), "replica identity")
}
