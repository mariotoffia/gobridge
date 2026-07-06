package paho

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestFactory_Capabilities_AdvertisesSharedConsumer verifies the MQTT
// factory advertises CapSharedConsumer (finding 9). MQTT shared
// subscriptions ($share/<group>/<filter>) are supported by the router
// (topic_match.go strips the $share prefix), so the capability must be
// discoverable by the runtime/operators.
func TestFactory_Capabilities_AdvertisesSharedConsumer(t *testing.T) {
	f := NewFactory(nil)

	require.Contains(t, f.Capabilities(), ports.CapSharedConsumer,
		"MQTT transport must advertise shared-consumer support")
}
