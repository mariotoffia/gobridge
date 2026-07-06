package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// TestEnvelopeFromPublish_RetainedAndQoSHeaders verifies the inbound
// conversion records the MQTT retained flag and QoS level under the
// reserved mqtt.* header namespace (finding 8). Without the fix these
// transport facts were dropped and never reached the pipeline.
func TestEnvelopeFromPublish_RetainedAndQoSHeaders(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "sensors/temp",
		QoS:     2,
		Retain:  true,
		Payload: []byte("p"),
	}

	env := EnvelopeFromPublish(pub, nil)

	require.Equal(t, true, env.Headers()[HeaderMQTTRetained],
		"retained flag must be mapped to %q", HeaderMQTTRetained)
	require.Equal(t, 2, env.Headers()[HeaderMQTTQoS],
		"delivery QoS must be mapped to %q", HeaderMQTTQoS)
}

// TestEnvelopeFromPublish_RetainedQoS_SpoofStripped verifies inbound
// user properties named mqtt.retained / mqtt.qos cannot override the
// adapter-controlled transport facts (mirrors the mqtt.topic guard).
func TestEnvelopeFromPublish_RetainedQoS_SpoofStripped(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "sensors/temp",
		QoS:     1,
		Retain:  false,
		Payload: []byte("p"),
		Properties: &pahov5.PublishProperties{
			User: pahov5.UserProperties{
				{Key: HeaderMQTTRetained, Value: "true"},
				{Key: HeaderMQTTQoS, Value: "9"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	require.Equal(t, false, env.Headers()[HeaderMQTTRetained],
		"inbound user property must not spoof %q", HeaderMQTTRetained)
	require.Equal(t, 1, env.Headers()[HeaderMQTTQoS],
		"inbound user property must not spoof %q", HeaderMQTTQoS)
}
