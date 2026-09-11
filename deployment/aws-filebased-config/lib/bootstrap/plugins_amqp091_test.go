//go:build gobridge_amqp091 || gobridge_all

package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/ports"
)

func init() {
	linkedOptionalKinds["amqp091"] = true
	linkedOptionalKinds["amqp.amqp091"] = true
}

// TestPluginRegistry_DecodesAMQP091Kinds proves selecting the AMQP 0-9-1
// family makes both of its discriminators decodable in the profile binary.
func TestPluginRegistry_DecodesAMQP091Kinds(t *testing.T) {
	kinds := newDefaultPluginRegistry().Kinds()

	assert.Contains(t, kinds, "amqp091")
	assert.Contains(t, kinds, "amqp.amqp091")
}

// TestFactoryRegistry_AMQP091AliasesShareOneFactory proves the selected
// AMQP 0-9-1 family is wired for both discriminators, and that the
// alias resolves to the same factory instance rather than a second transport
// holding its own connection state.
func TestFactoryRegistry_AMQP091AliasesShareOneFactory(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	short, ok := reg.transports["amqp091"]
	require.True(t, ok, "amqp091 transport factory must be wired when the family is selected")
	qualified, ok := reg.transports["amqp.amqp091"]
	require.True(t, ok, "amqp.amqp091 transport factory must be wired when the family is selected")
	require.Same(t, short, qualified, "alias must resolve to the same factory instance")
}

// TestFactoryRegistry_AMQP091ExclusiveReceiverSerializesTheSwap proves the real
// factory's exclusivity hook is reachable through this root's swap-mode
// detection. The factory is rebuilt for every plan, so its post-build
// capability latch is always cold here — the receiver config is the only
// signal that overlapping a swap would put two consumers on one exclusive
// queue, which the broker refuses.
func TestFactoryRegistry_AMQP091ExclusiveReceiverSerializesTheSwap(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	cfg := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "sess", Transport: "amqp091"}},
		Receivers: []ports.ReceiverDef{{
			ID:        "rx",
			SessionID: "sess",
			Config:    &amqp091.Config{Receiver: amqp091.ReceiverParams{QueueName: "q", Exclusive: true}},
		}},
	}

	reg := app.newFactoryRegistry(cfg)

	require.Equal(t, swapModePrepareCommit, reg.detectSwapMode(nil, cfg))
}

// TestFactoryRegistry_AMQP091LeavingExclusiveSerializesTheSwap covers the reverse
// transition. The incoming receiver is no longer exclusive, so nothing in the
// new config claims an identity — but the running exclusive consumer is still
// attached when an overlapping swap starts the new one, and RabbitMQ refuses a
// second consumer on a queue in exclusive use. The factory is fresh for this
// plan, so no capability latch remembers the old consumer either.
func TestFactoryRegistry_AMQP091LeavingExclusiveSerializesTheSwap(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	receiver := func(exclusive bool) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Sessions: []ports.SessionDef{{ID: "sess", Transport: "amqp091"}},
			Receivers: []ports.ReceiverDef{{
				ID:        "rx",
				SessionID: "sess",
				Config:    &amqp091.Config{Receiver: amqp091.ReceiverParams{QueueName: "q", Exclusive: exclusive}},
			}},
		}
	}
	running, next := receiver(true), receiver(false)

	reg := app.newFactoryRegistry(next)

	require.Equal(t, swapModePrepareCommit, reg.detectSwapMode(running, next))
}
