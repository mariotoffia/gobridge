package bridge

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The two digest contracts are ONE identity now: the content normal form leaves
// the version number out, so the deployment baseline and the rollout artifact
// recognize a document by exactly the same value. What they recognize is the
// content — a real edit still gives a different digest, and neither call touches
// the config it is handed.
func TestBaselineDigests_AreOneVersionIndependentContentIdentity(t *testing.T) {
	cfg := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", KeepAlive: 30})
	want, err := DeploymentBaselineContentDigest(cfg)
	require.NoError(t, err)
	for _, version := range []int{0, 1, 8, 99} {
		cfg.Version = version
		artifact, err := ConfigArtifactDigest(cfg)
		require.NoError(t, err)
		baseline, err := DeploymentBaselineContentDigest(cfg)
		require.NoError(t, err)
		assert.Equal(t, artifact, baseline, "both calls answer with the same content identity")
		assert.Equal(t, want, baseline, "the version number is not part of the content")
		assert.Equal(t, version, cfg.Version, "recognition must not mutate the actual config")
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
