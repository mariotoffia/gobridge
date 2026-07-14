package amqp091

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FreezePluginConfig_PreservesCredentialRollback(t *testing.T) {
	cfg := &Config{
		CredentialsURIRef: "vault://amqp091",
		Session:           SessionOptions{TLS: &TLSConfig{CACertFile: "ca.pem"}},
		Subscription:      SubscriptionParams{QueueArguments: map[string]any{"nested": map[string]any{"limit": 1}}},
	}
	freezer, ok := any(cfg).(ports.FreezableConfig)
	require.True(t, ok)
	frozen := freezer.FreezePluginConfig().(*Config)
	password := connectivity.NewPasswordCredential("resolved-user", "resolved-secret")
	require.NoError(t, frozen.ApplyCredentials(connectivity.NewCredentialSet(&password, nil)))
	assert.Equal(t, "vault://amqp091", cfg.CredentialsURIRef)
	assert.Empty(t, frozen.CredentialsURIRef)
	assert.Empty(t, cfg.Session.Username)
	assert.Equal(t, "resolved-user", frozen.Session.Username)
	frozen.Session.TLS.CACertFile = "changed.pem"
	frozen.Subscription.QueueArguments["nested"].(map[string]any)["limit"] = 2
	assert.Equal(t, "ca.pem", cfg.Session.TLS.CACertFile)
	assert.Equal(t, 1, cfg.Subscription.QueueArguments["nested"].(map[string]any)["limit"])
}
