package bootstrap

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestMQTTMemoryProfile_DerivesLargestSafeReceiveMaximum(t *testing.T) {
	cfg, sessions := mqttMemoryProfileConfig(1, 0)
	bootstrapCfg := deployinfra.BootstrapConfig{ContainerMemoryBytes: 1 << 30}

	require.NoError(t, applyMQTTMemoryProfile(cfg, bootstrapCfg))

	const wantBudget = uint64(256 << 20)
	wantReceive, err := paho.LargestSafeReceiveMaximum(
		paho.DefaultMaxPayloadBytes, wantBudget, routing.DefaultMaxInFlight,
	)
	require.NoError(t, err)
	assert.Equal(t, wantBudget, sessions[0].Session.IngressMemoryBudgetBytes)
	assert.Equal(t, wantReceive, sessions[0].Session.ReceiveMaximum)

	bound, err := paho.IngressMemoryBound(
		sessions[0].Session.MaxPayloadBytes,
		sessions[0].Session.ReceiveMaximum,
		routing.DefaultMaxInFlight,
	)
	require.NoError(t, err)
	assert.LessOrEqual(t, bound, wantBudget)
}

func TestMQTTMemoryProfile_DividesReservationAcrossIngressSessions(t *testing.T) {
	cfg, sessions := mqttMemoryProfileConfig(2, 0)

	require.NoError(t, applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))

	for _, session := range sessions {
		assert.Equal(t, uint64(128<<20), session.Session.IngressMemoryBudgetBytes)
	}
}

func TestMQTTMemoryProfile_SenderOnlySessionsDoNotConsumeIngressAllocation(t *testing.T) {
	cfg, sessions := mqttMemoryProfileConfig(1, 1)

	require.NoError(t, applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))

	assert.Equal(t, uint64(256<<20), sessions[0].Session.IngressMemoryBudgetBytes)
	assert.Equal(t, paho.DefaultIngressMemoryBudgetBytes, sessions[1].Session.IngressMemoryBudgetBytes)
	assert.Equal(t, paho.DefaultReceiveMaximum, sessions[1].Session.ReceiveMaximum)
}

func TestMQTTMemoryProfile_HeadroomExactBoundaryAcceptsAndOneByteExcessRejects(t *testing.T) {
	const container = uint64(1 << 30)
	const ingress = container / 4
	const allowed = container * 4 / 5
	cfg, _ := mqttMemoryProfileConfig(1, 0)

	err := applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: container,
		ReservedMemoryBytes:  allowed - ingress,
	})
	require.NoError(t, err)

	cfg, _ = mqttMemoryProfileConfig(1, 0)
	err = applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: container,
		ReservedMemoryBytes:  allowed - ingress + 1,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestMQTTMemoryProfile_RejectsZeroOverflowNoIngressAndImpossibleInputs(t *testing.T) {
	t.Run("zero container memory", func(t *testing.T) {
		cfg, _ := mqttMemoryProfileConfig(1, 0)
		err := applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{})
		require.Error(t, err)
		assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	})
	t.Run("reserved memory overflow", func(t *testing.T) {
		cfg, _ := mqttMemoryProfileConfig(1, 0)
		err := applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
			ContainerMemoryBytes: 1 << 30,
			ReservedMemoryBytes:  math.MaxUint64,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	})
	t.Run("no ingress", func(t *testing.T) {
		_, err := mqttMemoryAllocation(1<<30, 0, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	})
	t.Run("budget cannot fit one packet window", func(t *testing.T) {
		cfg, _ := mqttMemoryProfileConfig(1, 0)
		err := applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
			ContainerMemoryBytes: 1 << 20,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	})
}

func TestMQTTMemoryProfile_ExplicitUnsafeConfigRejectsInsteadOfClamping(t *testing.T) {
	cfg, sessions := mqttMemoryProfileConfig(1, 0)
	sessions[0].Session.ReceiveMaximum = math.MaxUint16

	err := applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.Equal(t, uint16(math.MaxUint16), sessions[0].Session.ReceiveMaximum)
}

func mqttMemoryProfileConfig(ingressSessions, senderOnlySessions int) (
	*ports.BridgeConfig,
	[]*paho.Config,
) {
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "aws-mqtt-memory"}}
	configs := make([]*paho.Config, 0, ingressSessions+senderOnlySessions)
	for i := range ingressSessions {
		sessionCfg := paho.DefaultConfig()
		sessionID := "ingress-" + string(rune('a'+i))
		receiverID := "receiver-" + string(rune('a'+i))
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
			ID: sessionID, Transport: "mqtt", Config: &sessionCfg,
		})
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{
			ID: receiverID, Transport: "mqtt", SessionID: sessionID,
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: "route-" + string(rune('a'+i)), ReceiverID: receiverID,
		})
		configs = append(configs, &sessionCfg)
	}
	for i := range senderOnlySessions {
		sessionCfg := paho.DefaultConfig()
		sessionID := "sender-" + string(rune('a'+i))
		cfg.Sessions = append(cfg.Sessions, ports.SessionDef{
			ID: sessionID, Transport: "mqtt", Config: &sessionCfg,
		})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{
			ID: "sender-" + string(rune('a'+i)), Transport: "mqtt", SessionID: sessionID,
		})
		configs = append(configs, &sessionCfg)
	}
	return cfg, configs
}
