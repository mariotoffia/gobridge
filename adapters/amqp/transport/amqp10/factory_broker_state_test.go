package amqp10

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// brokerStateSpecs returns the session spec and the receiver specs of session
// "orders" at address with containerID, one receiver per params.
func brokerStateSpecs(address, containerID string, params ...ReceiverParams) (ports.SessionSpec, []ports.ReceiverSpec) {
	session := ports.SessionSpec{ID: "orders", Config: &Config{Session: SessionOptions{Address: address, ContainerID: containerID}}}
	receivers := make([]ports.ReceiverSpec, 0, len(params))
	for i, p := range params {
		receivers = append(receivers, ports.ReceiverSpec{ID: fmt.Sprintf("rx-%d", i), SessionID: "orders", Config: &Config{Receiver: p}})
	}
	return session, receivers
}

func durableTopic(address, subscriptionName string) ReceiverParams {
	return ReceiverParams{Address: address, DurabilityMode: 2, Routing: RoutingMulticast, SubscriptionName: subscriptionName}
}

// requireKeys returns the broker state keys of brokerStateSpecs(address,
// containerID, params...) and fails the test on an error.
func requireKeys(t *testing.T, address, containerID string, params ...ReceiverParams) []string {
	t.Helper()
	keys, err := NewFactory(nil).BrokerStateKeys(brokerStateSpecs(address, containerID, params...))
	require.NoError(t, err)
	return keys
}

func TestFactoryBrokerStateKeys_KeysADurableTopicReceiver(t *testing.T) {
	keys := requireKeys(t, "amqp://broker.example:5672", "bridge-a", durableTopic("orders", "orders-sub"))

	require.Len(t, keys, 1)
	assert.True(t, strings.HasPrefix(keys[0], "amqp10:"), "the key names its transport")
	assert.NotContains(t, keys[0], "bridge-a")
	assert.NotContains(t, keys[0], "orders-sub")
}

func TestFactoryBrokerStateKeys_QueueReceiverHasNoKey(t *testing.T) {
	queue := ReceiverParams{Address: "orders", DurabilityMode: 2, Routing: RoutingAnycast}

	assert.Empty(t, requireKeys(t, "amqp://broker.example:5672", "bridge-a", queue))
}

func TestFactoryBrokerStateKeys_NonDurableTopicReceiverHasNoKey(t *testing.T) {
	topic := ReceiverParams{Address: "orders", Routing: RoutingMulticast}

	assert.Empty(t, requireKeys(t, "amqp://broker.example:5672", "bridge-a", topic))
}

func TestFactoryBrokerStateKeys_SpellingsOfOneBrokerShareAKey(t *testing.T) {
	sub := durableTopic("orders", "orders-sub")
	for _, pair := range [][2]string{
		{"amqp://broker.example:5672", "amqp://Broker.Example"},
		{"amqps://broker.example:5671", "amqp+ssl://broker.example"},
		{"amqp://broker.example:5672", "amqp://user:secret@broker.example:5672"},
	} {
		assert.Equal(t,
			requireKeys(t, pair[0], "bridge-a", sub),
			requireKeys(t, pair[1], "bridge-a", sub),
			"%s and %s", pair[0], pair[1])
	}
}

func TestFactoryBrokerStateKeys_KeyFollowsTheContainerIDAndTheLinkName(t *testing.T) {
	want := requireKeys(t, "amqp://broker.example:5672", "bridge-a", durableTopic("orders", ""))

	assert.Equal(t, want,
		requireKeys(t, "amqp://broker.example:5672", "bridge-a", durableTopic("orders", "bridge-a:orders")),
		"an explicit subscription_name equal to the derived link name is the same subscription")
	assert.NotEqual(t, want,
		requireKeys(t, "amqp://broker.example:5672", "bridge-b", durableTopic("orders", "")),
		"container_id")
	assert.NotEqual(t, want,
		requireKeys(t, "amqp://broker.example:5672", "bridge-a", durableTopic("orders", "renamed")),
		"subscription_name")
	assert.NotEqual(t, want,
		requireKeys(t, "amqp://other.example:5672", "bridge-a", durableTopic("orders", "")),
		"broker")
}

func TestFactoryBrokerStateKeys_RejectsADurableTopicReceiverWithoutAContainerID(t *testing.T) {
	session, receivers := brokerStateSpecs("amqp://broker.example:5672", "", durableTopic("orders", "orders-sub"))

	_, err := NewFactory(nil).BrokerStateKeys(session, receivers)

	require.Error(t, err)
}
