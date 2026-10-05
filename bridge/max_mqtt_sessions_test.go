package bridge_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestPreflight_MQTTSessionsAtLimitAccepted(t *testing.T) {
	cfg := mqttSessionCountConfig(3, 3, 0)

	require.NoError(t, bridge.NewBuilder(cfg).Preflight(t.Context()))
}

func TestPreflight_MQTTSessionsOverLimitRefused(t *testing.T) {
	cfg := mqttSessionCountConfig(3, 4, 0)

	err := bridge.NewBuilder(cfg).Preflight(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	var bridgeErr *shared.BridgeError
	require.True(t, errors.As(err, &bridgeErr))
	assert.Equal(t, shared.ErrInvalidConfig.Code, bridgeErr.Code)
	assert.Equal(t, shared.ErrInvalidConfig.Class, bridgeErr.Class)
	assert.Contains(t, bridgeErr.Message,
		"bridge.max_mqtt_sessions: the configuration uses 4 MQTT sessions, more than the limit of 3",
		"the message names how many MQTT sessions the configuration uses and the limit")
}

func TestPreflight_MQTTSessionsUnlimitedWhenLimitAbsent(t *testing.T) {
	cfg := mqttSessionCountConfig(0, 50, 0)

	require.NoError(t, bridge.NewBuilder(cfg).Preflight(t.Context()))
}

func TestPreflight_MQTTSessionLimitIgnoresOtherTransports(t *testing.T) {
	cfg := mqttSessionCountConfig(1, 1, 2)

	require.NoError(t, bridge.NewBuilder(cfg).Preflight(t.Context()))
}

func TestPreflight_UnreferencedMQTTSessionNotCounted(t *testing.T) {
	cfg := mqttSessionCountConfig(2, 2, 0)
	cfg.Sessions = append(cfg.Sessions, ports.SessionDef{ID: "mqtt-unreferenced", Transport: "mqtt"})

	require.NoError(t, bridge.NewBuilder(cfg).Preflight(t.Context()),
		"a session nothing references is never built, so it does not count toward the limit")
}

// mqttSessionCountConfig defines mqtt sessions over both MQTT transport names
// and other sessions on a non-MQTT transport, each referenced by a sender so
// the builder would build it, under bridge.max_mqtt_sessions.
func mqttSessionCountConfig(limit, mqtt, other int) *ports.BridgeConfig {
	sessions := make([]ports.SessionDef, 0, mqtt+other)
	senders := make([]ports.SenderDef, 0, mqtt+other)
	add := func(id, transport string) {
		sessions = append(sessions, ports.SessionDef{ID: id, Transport: transport})
		senders = append(senders, ports.SenderDef{ID: "sender-" + id, Transport: transport, SessionID: id})
	}
	for i := range mqtt {
		transport := "mqtt"
		if i%2 == 1 {
			transport = "mqtt.paho"
		}
		add(fmt.Sprintf("mqtt-%d", i), transport)
	}
	for i := range other {
		add(fmt.Sprintf("other-%d", i), "amqp.amqp091")
	}
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "mqtt-session-count", MaxMQTTSessions: limit},
		Sessions: sessions,
		Senders:  senders,
	}
}
