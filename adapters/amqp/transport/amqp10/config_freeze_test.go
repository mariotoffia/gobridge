package amqp10

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FreezePluginConfig_PreservesCredentialRollback(t *testing.T) {
	durable := true
	cfg := &Config{
		CredentialsURIRef: "vault://amqp10",
		Session:           SessionOptions{Address: "amqps://broker.example", TLS: &TLSConfig{CACertFile: "ca.pem"}},
		Sender:            SenderParams{Durable: &durable},
	}
	freezer, ok := any(cfg).(ports.FreezableConfig)
	require.True(t, ok)
	frozen := freezer.FreezePluginConfig().(*Config)
	password := connectivity.NewPasswordCredential("resolved-user", "resolved-secret")
	require.NoError(t, frozen.ApplyCredentials(connectivity.NewCredentialSet(&password, nil)))
	assert.Equal(t, "vault://amqp10", cfg.CredentialsURIRef)
	assert.Empty(t, frozen.CredentialsURIRef)
	assert.Empty(t, cfg.Session.Username)
	assert.Equal(t, "resolved-user", frozen.Session.Username)
	frozen.Session.TLS.CACertFile = "changed.pem"
	*frozen.Sender.Durable = false
	assert.Equal(t, "ca.pem", cfg.Session.TLS.CACertFile)
	assert.True(t, *cfg.Sender.Durable)
}
