package sqs

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FreezePluginConfig_PreservesCredentialRollback(t *testing.T) {
	autoExtend := true
	cfg := &Config{CredentialsURIRef: "vault://sqs", AutoExtend: &autoExtend}
	freezer, ok := any(cfg).(ports.FreezableConfig)
	require.True(t, ok)
	frozen := freezer.FreezePluginConfig().(*Config)
	password := connectivity.NewPasswordCredential("resolved-user", "resolved-secret")
	require.NoError(t, frozen.ApplyCredentials(connectivity.NewCredentialSet(&password, nil)))
	assert.Equal(t, "vault://sqs", cfg.CredentialsURIRef)
	assert.Empty(t, frozen.CredentialsURIRef)
	assert.Nil(t, cfg.resolvedCreds)
	require.NotNil(t, frozen.resolvedCreds)
	assert.Equal(t, "resolved-user", frozen.resolvedCreds.Username())
	*frozen.AutoExtend = false
	assert.True(t, *cfg.AutoExtend)
}
