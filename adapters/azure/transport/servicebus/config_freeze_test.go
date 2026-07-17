package servicebus

import (
	"crypto/tls"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_FreezePluginConfig_PreservesCredentialRollback(t *testing.T) {
	autoExtend := true
	cfg := &Config{
		CredentialsURIRef: "vault://servicebus",
		Receiver:          ReceiverParams{AutoExtend: &autoExtend},
		Connection:        ConnectionConfig{TLSConfig: &tls.Config{ServerName: "broker.example", NextProtos: []string{"h2"}, Certificates: []tls.Certificate{{Certificate: [][]byte{{1, 2, 3}}}}}},
	}
	freezer, ok := any(cfg).(ports.FreezableConfig)
	require.True(t, ok)
	frozen := freezer.FreezePluginConfig().(*Config)
	password := connectivity.NewPasswordCredential("resolved-user", "resolved-secret")
	require.NoError(t, frozen.ApplyCredentials(connectivity.NewCredentialSet(&password, nil)))
	assert.Equal(t, "vault://servicebus", cfg.CredentialsURIRef)
	assert.Empty(t, frozen.CredentialsURIRef)
	assert.Empty(t, cfg.Connection.ClientID)
	assert.Equal(t, "resolved-user", frozen.Connection.ClientID)
	*frozen.Receiver.AutoExtend = false
	frozen.Connection.TLSConfig.ServerName = "changed.example"
	frozen.Connection.TLSConfig.NextProtos[0] = "changed"
	frozen.Connection.TLSConfig.Certificates[0].Certificate[0][0] = 9
	assert.True(t, *cfg.Receiver.AutoExtend)
	assert.Equal(t, "broker.example", cfg.Connection.TLSConfig.ServerName)
	assert.Equal(t, "h2", cfg.Connection.TLSConfig.NextProtos[0])
	assert.Equal(t, byte(1), cfg.Connection.TLSConfig.Certificates[0].Certificate[0][0])
}
