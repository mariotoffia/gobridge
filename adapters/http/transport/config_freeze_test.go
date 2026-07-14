package transport

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FreezePluginConfig_PreservesCredentialRollback(t *testing.T) {
	cfg := &Config{CredentialsURIRef: "vault://http", Path: "/events"}
	freezer, ok := any(cfg).(ports.FreezableConfig)
	require.True(t, ok)
	frozen := freezer.FreezePluginConfig().(*Config)
	password := connectivity.NewPasswordCredential("resolved-user", "resolved-secret")
	require.NoError(t, frozen.ApplyCredentials(connectivity.NewCredentialSet(&password, nil)))
	assert.Equal(t, "vault://http", cfg.CredentialsURIRef)
	assert.Empty(t, frozen.CredentialsURIRef)
	assert.True(t, cfg.APIKey.IsZero())
	assert.Equal(t, "resolved-secret", frozen.APIKey.Reveal())
	frozen.Path = "/changed"
	assert.Equal(t, "/events", cfg.Path)
}
