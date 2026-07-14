package validate_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replicaIdentityTestConfig struct{ strategy string }

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
