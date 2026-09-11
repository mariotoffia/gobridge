//go:build gobridge_amqp10 || gobridge_all

package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	linkedOptionalKinds["amqp10"] = true
	linkedOptionalKinds["amqp.amqp10"] = true
}

// TestPluginRegistry_DecodesAMQP10Kinds proves selecting the AMQP 1.0
// family makes both of its discriminators decodable in the profile binary.
func TestPluginRegistry_DecodesAMQP10Kinds(t *testing.T) {
	kinds := newDefaultPluginRegistry().Kinds()

	assert.Contains(t, kinds, "amqp10")
	assert.Contains(t, kinds, "amqp.amqp10")
}

// TestFactoryRegistry_AMQP10AliasesShareOneFactory proves the selected
// AMQP 1.0 family is wired for both discriminators, and that the
// alias resolves to the same factory instance rather than a second transport
// holding its own connection state.
func TestFactoryRegistry_AMQP10AliasesShareOneFactory(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	short, ok := reg.transports["amqp10"]
	require.True(t, ok, "amqp10 transport factory must be wired when the family is selected")
	qualified, ok := reg.transports["amqp.amqp10"]
	require.True(t, ok, "amqp.amqp10 transport factory must be wired when the family is selected")
	require.Same(t, short, qualified, "alias must resolve to the same factory instance")
}
