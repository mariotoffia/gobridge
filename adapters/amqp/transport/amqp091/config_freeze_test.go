package amqp091

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	amqp "github.com/rabbitmq/amqp091-go"
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

func TestConfig_FreezePluginConfig_DeepClonesNestedAMQPTableValues(t *testing.T) {
	nested := amqp.Table{
		"bytes": []byte{1, 2},
		"array": []any{
			amqp.Table{"deep": []byte{3, 4}},
			[]byte{5, 6},
		},
	}
	cfg := Config{Subscription: SubscriptionParams{
		QueueArguments: map[string]any{"nested": nested},
	}}

	frozen := cfg.FreezePluginConfig().(*Config)

	nested["bytes"].([]byte)[0] = 9
	array := nested["array"].([]any)
	array[0].(amqp.Table)["deep"].([]byte)[0] = 8
	array[1].([]byte)[0] = 7
	array[0] = "replaced"
	nested["new"] = []byte{10}

	frozenNested := frozen.Subscription.QueueArguments["nested"].(amqp.Table)
	assert.Equal(t, []byte{1, 2}, frozenNested["bytes"])
	frozenArray := frozenNested["array"].([]any)
	assert.Equal(t, []byte{3, 4}, frozenArray[0].(amqp.Table)["deep"])
	assert.Equal(t, []byte{5, 6}, frozenArray[1])
	assert.NotContains(t, frozenNested, "new")
}
