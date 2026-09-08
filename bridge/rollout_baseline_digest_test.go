package bridge

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func TestDeploymentBaselineContentDigest_IgnoresOnlySourceVersion(t *testing.T) {
	cfg := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", KeepAlive: 30})
	want, err := DeploymentBaselineContentDigest(cfg)
	require.NoError(t, err)
	artifacts := map[string]bool{}
	for _, version := range []int{0, 1, 8, 99} {
		cfg.Version = version
		before, err := ConfigArtifactDigest(cfg)
		require.NoError(t, err)
		got, err := DeploymentBaselineContentDigest(cfg)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.Equal(t, version, cfg.Version, "recognition must not mutate the actual config")
		after, err := ConfigArtifactDigest(cfg)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		assert.False(t, artifacts[after], "artifact identity must still distinguish version %d", version)
		artifacts[after] = true
	}

	for name, change := range map[string]func(*ports.BridgeConfig){
		"log level": func(c *ports.BridgeConfig) { c.Bridge.LogLevel = "debug" },
		"route":     func(c *ports.BridgeConfig) { c.Routes = []ports.RouteDef{{ID: "added"}} },
		"plugin": func(c *ports.BridgeConfig) {
			c.Sessions[0].Config.(*roundTripConfig).KeepAlive = 60
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", KeepAlive: 30})
			changed.Version = 100
			change(changed)
			got, err := DeploymentBaselineContentDigest(changed)
			require.NoError(t, err)
			assert.NotEqual(t, want, got, "editable content is part of the deployment baseline")
		})
	}
}

func TestBaselineDigests_RejectUncanonicalizableConfig(t *testing.T) {
	malformed := &ports.BridgeConfig{Routes: []ports.RouteDef{{
		Policy: ports.PolicyDef{Backoff: ports.BackoffDef{Multiplier: math.NaN()}},
	}}}
	for name, digest := range map[string]func(*ports.BridgeConfig) (string, error){
		"artifact": ConfigArtifactDigest,
		"content":  DeploymentBaselineContentDigest,
	} {
		t.Run(name, func(t *testing.T) {
			for _, cfg := range []*ports.BridgeConfig{nil, malformed} {
				got, err := digest(cfg)
				assert.Error(t, err)
				assert.Empty(t, got)
			}
		})
	}
}
