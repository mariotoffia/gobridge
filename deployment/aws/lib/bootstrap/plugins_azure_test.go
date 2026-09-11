//go:build gobridge_azure || gobridge_all

package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	linkedOptionalKinds["servicebus"] = true
	linkedOptionalKinds["azure.servicebus"] = true
}

// TestPluginRegistry_DecodesAzureKinds proves selecting the Azure Service Bus
// family makes both of its discriminators decodable in the profile binary.
func TestPluginRegistry_DecodesAzureKinds(t *testing.T) {
	kinds := newDefaultPluginRegistry().Kinds()

	assert.Contains(t, kinds, "servicebus")
	assert.Contains(t, kinds, "azure.servicebus")
}

// TestFactoryRegistry_AzureAliasesShareOneFactory proves the selected
// Azure Service Bus family is wired for both discriminators, and that the
// alias resolves to the same factory instance rather than a second transport
// holding its own connection state.
func TestFactoryRegistry_AzureAliasesShareOneFactory(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	short, ok := reg.transports["servicebus"]
	require.True(t, ok, "servicebus transport factory must be wired when the family is selected")
	qualified, ok := reg.transports["azure.servicebus"]
	require.True(t, ok, "azure.servicebus transport factory must be wired when the family is selected")
	require.Same(t, short, qualified, "alias must resolve to the same factory instance")
}

// TestFactoryRegistry_AzurePinnedSessionSerializesTheSwap proves a receiver
// pinned to one Service Bus session reaches this root's swap-mode detection.
// Service Bus advertises no exclusive-identity capability — the pin lives in
// one receiver's config — so overlapping the swap would leave the incoming
// receiver retrying session-cannot-be-locked against the outgoing runtime's
// drain, and failing its route when the retries run out first.
func TestFactoryRegistry_AzurePinnedSessionSerializesTheSwap(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	cfg := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "sess", Transport: "servicebus"}},
		Receivers: []ports.ReceiverDef{{
			ID:        "rx",
			SessionID: "sess",
			Config: &servicebus.Config{Receiver: servicebus.ReceiverParams{
				QueueName: "q", SessionID: "pinned-1",
			}},
		}},
	}

	reg := app.newFactoryRegistry(cfg)

	require.Equal(t, swapModePrepareCommit, reg.detectSwapMode(nil, cfg))
}
