package bootstrap

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/connectivity"
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

func TestMQTTMemoryProfile_CloneParsePreservesDefaultAndExplicitReceiveMaximum(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-clone
sessions:
  - id: ingress-default
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress-default
  - id: ingress-explicit
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress-explicit
        receive_maximum: 100
receivers:
  - id: receiver-default
    transport: mqtt
    session_id: ingress-default
    topics:
      - topic: memory/default
        qos: 1
  - id: receiver-explicit
    transport: mqtt
    session_id: ingress-explicit
    topics:
      - topic: memory/explicit
        qos: 1
routes:
  - id: route-default
    receiver_id: receiver-default
    policy:
      max_in_flight: 1
  - id: route-explicit
    receiver_id: receiver-explicit
    policy:
      max_in_flight: 1
`
	registry := newDefaultPluginRegistry()
	logical, err := cfgparser.Parse(strings.NewReader(configYAML), cfgparser.FormatYAML, registry)
	require.NoError(t, err)
	inputs, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}, deployinfra.BootstrapConfig{
		AdminAPIKeyParam:     "/admin",
		ContainerMemoryBytes: 1 << 30,
	}, registry, logical)
	require.NoError(t, err)

	require.NoError(t, applyMQTTMemoryProfile(inputs.RuntimeConfig, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))

	defaultConfig, ok := inputs.RuntimeConfig.Sessions[0].Config.(*paho.Config)
	require.True(t, ok)
	explicitConfig, ok := inputs.RuntimeConfig.Sessions[1].Config.(*paho.Config)
	require.True(t, ok)
	wantDerived, err := paho.LargestSafeReceiveMaximum(
		paho.DefaultMaxPayloadBytes,
		128<<20,
		1,
	)
	require.NoError(t, err)
	assert.Equal(t, wantDerived, defaultConfig.Session.ReceiveMaximum)
	assert.Equal(t, uint16(100), explicitConfig.Session.ReceiveMaximum)
	assert.Equal(t, uint64(128<<20), defaultConfig.Session.IngressMemoryBudgetBytes)
	assert.Equal(t, uint64(128<<20), explicitConfig.Session.IngressMemoryBudgetBytes)
}

func TestMQTTMemoryProfile_RealYAMLOmittedReceiveMaximumDerivesForOneMiBPayload(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-one-mib
sessions:
  - id: ingress
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
        max_payload_bytes: 1048576
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: memory/one-mib
        qos: 1
routes:
  - id: route
    receiver_id: receiver
    policy:
      max_in_flight: 1
`
	cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
	session := mqttProfileSessionConfig(t, cfg, "ingress")
	wantReceive, err := paho.LargestSafeReceiveMaximum(1<<20, 256<<20, 1)
	require.NoError(t, err)
	assert.Equal(t, wantReceive, session.Session.ReceiveMaximum)
	assert.Less(t, session.Session.ReceiveMaximum, paho.DefaultReceiveMaximum)
}

func TestMQTTMemoryProfile_RealYAMLExplicitReceiveMaximum192RejectsOneMiBPayload(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-explicit-one-mib
sessions:
  - id: ingress
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
        max_payload_bytes: 1048576
        receive_maximum: 192
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: memory/explicit
        qos: 1
`
	registry := newDefaultPluginRegistry()
	_, err := cfgparser.Parse(strings.NewReader(configYAML), cfgparser.FormatYAML, registry)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestMQTTMemoryProfile_RealYAMLGenericBridgePreflightUsesDefaultReceiveMaximum(t *testing.T) {
	const configYAML = `
bridge:
  id: generic-mqtt-one-mib
sessions:
  - id: ingress
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
        max_payload_bytes: 1048576
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: memory/generic
        qos: 1
routes:
  - id: route
    receiver_id: receiver
    policy:
      max_in_flight: 1
`
	registry := newDefaultPluginRegistry()
	logical, err := cfgparser.Parse(strings.NewReader(configYAML), cfgparser.FormatYAML, registry)
	require.NoError(t, err, "omitted receive maximum must survive parser validation")

	_, err = bridge.NewBuilder(logical).
		RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
		Plan(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig,
		"generic preflight must apply default Receive Maximum 192 before resources")
}

func TestMQTTMemoryProfile_RealYAMLPayloadRegressionsParseBeforeProfile(t *testing.T) {
	for _, maxPayload := range []string{"262144", "2097152"} {
		t.Run(maxPayload, func(t *testing.T) {
			configYAML := `
bridge:
  id: aws-mqtt-payload-regression
sessions:
  - id: ingress
    transport: mqtt
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
        max_payload_bytes: ` + maxPayload + `
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: memory/regression
        qos: 1
`
			cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
			session := mqttProfileSessionConfig(t, cfg, "ingress")
			assert.NotZero(t, session.Session.ReceiveMaximum)
		})
	}
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

func TestMQTTMemoryProfile_EphemeralSenderOnlySessionsDoNotConsumeIngressAllocation(t *testing.T) {
	cfg, sessions := mqttMemoryProfileConfig(1, 1)

	require.NoError(t, applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))

	assert.Equal(t, uint64(256<<20), sessions[0].Session.IngressMemoryBudgetBytes)
	assert.Zero(t, sessions[1].Session.IngressMemoryBudgetBytes)
	assert.Zero(t, sessions[1].Session.ReceiveMaximum,
		"sender-only sessions are not normalized or allocated as ingress")
}

func TestMQTTMemoryProfile_RealParseEphemeralReceiverWithoutRouteConsumesAllocation(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-unconsumed-receiver
sessions:
  - id: ingress
    transport: mqtt
    session_mode: ephemeral
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ingress
receivers:
  - id: receiver
    transport: mqtt
    session_id: ingress
    topics:
      - topic: memory/unconsumed
        qos: 1
`
	cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
	session := mqttProfileSessionConfig(t, cfg, "ingress")
	assert.Equal(t, uint64(256<<20), session.Session.IngressMemoryBudgetBytes)
	assert.NotZero(t, session.Session.ReceiveMaximum)
}

func TestMQTTMemoryProfile_RealParseNoReceiverExcludesEphemeralSession(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-empty-session
sessions:
  - id: unused
    transport: mqtt
    session_mode: ephemeral
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: unused
`
	cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
	session := mqttProfileSessionConfig(t, cfg, "unused")
	assert.Zero(t, session.Session.IngressMemoryBudgetBytes)
	assert.Zero(t, session.Session.ReceiveMaximum)
}

func TestMQTTMemoryProfile_DurableSenderOnlySessionConsumesIngressAllocation(t *testing.T) {
	sessionCfg := paho.DefaultConfig()
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "durable-sender-memory"},
		Sessions: []ports.SessionDef{{
			ID:          "mqtt-durable-sender",
			Transport:   "mqtt",
			SessionMode: string(connectivity.SessionPersistent),
			Config:      &sessionCfg,
		}},
		Receivers: []ports.ReceiverDef{{
			ID:        "source",
			Transport: "sqs",
		}},
		Senders: []ports.SenderDef{{
			ID:        "mqtt-sender",
			Transport: "mqtt",
			SessionID: "mqtt-durable-sender",
		}},
		Bindings: []ports.BindingDef{{
			ID:       "mqtt-binding",
			SenderID: "mqtt-sender",
			Address:  "memory/out",
		}},
		Routes: []ports.RouteDef{{
			ID:         "route",
			ReceiverID: "source",
			Bindings:   []string{"mqtt-binding"},
		}},
	}

	require.NoError(t, applyMQTTMemoryProfile(cfg, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))
	assert.Equal(t, uint64(256<<20), sessionCfg.Session.IngressMemoryBudgetBytes)
	wantReceive, err := paho.LargestSafeReceiveMaximum(
		paho.DefaultMaxPayloadBytes,
		256<<20,
		0,
	)
	require.NoError(t, err)
	assert.Equal(t, wantReceive, sessionCfg.Session.ReceiveMaximum)
}

func TestMQTTMemoryProfile_RealParseDurableSenderOnlyWithoutRouteConsumesAllocation(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-durable-sender-only
sessions:
  - id: durable-publisher
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: durable-publisher
senders:
  - id: publisher
    session_id: durable-publisher
`
	cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
	session := mqttProfileSessionConfig(t, cfg, "durable-publisher")

	assert.Equal(t, uint64(256<<20), session.Session.IngressMemoryBudgetBytes)
	wantReceive, err := paho.LargestSafeReceiveMaximum(
		paho.DefaultMaxPayloadBytes,
		256<<20,
		0,
	)
	require.NoError(t, err)
	assert.Equal(t, wantReceive, session.Session.ReceiveMaximum)
}

func TestMQTTMemoryProfile_RealParseSenderAliasesDedupeDurableAndExcludeEphemeral(t *testing.T) {
	const configYAML = `
bridge:
  id: aws-mqtt-sender-aliases
sessions:
  - id: shared-durable
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: shared-durable
  - id: exclusive-publisher
    transport: mqtt
    session_mode: exclusive
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: exclusive-publisher
  - id: ephemeral-publisher
    transport: mqtt
    session_mode: ephemeral
    options:
      session:
        broker_url: tcp://broker:1883
        client_id: ephemeral-publisher
senders:
  - id: publisher-primary
    session_id: shared-durable
  - id: publisher-alias
    transport: mqtt
    session_id: shared-durable
  - id: sender-alias
    session_id: exclusive-publisher
  - id: ephemeral-alias
    session_id: ephemeral-publisher
`
	cfg := parseCloneAndApplyMQTTMemoryProfile(t, configYAML)
	shared := mqttProfileSessionConfig(t, cfg, "shared-durable")
	exclusive := mqttProfileSessionConfig(t, cfg, "exclusive-publisher")
	ephemeral := mqttProfileSessionConfig(t, cfg, "ephemeral-publisher")

	assert.Equal(t, uint64(128<<20), shared.Session.IngressMemoryBudgetBytes,
		"two sender aliases for one durable session consume one deduplicated share")
	assert.Equal(t, uint64(128<<20), exclusive.Session.IngressMemoryBudgetBytes)
	assert.Zero(t, ephemeral.Session.IngressMemoryBudgetBytes)
	assert.Zero(t, ephemeral.Session.ReceiveMaximum)
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

func parseCloneAndApplyMQTTMemoryProfile(t *testing.T, configYAML string) *ports.BridgeConfig {
	t.Helper()
	registry := newDefaultPluginRegistry()
	logical, err := cfgparser.Parse(strings.NewReader(configYAML), cfgparser.FormatYAML, registry)
	require.NoError(t, err)
	inputs, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin": "admin-secret-key-123456",
	}, deployinfra.BootstrapConfig{
		AdminAPIKeyParam:     "/admin",
		ContainerMemoryBytes: 1 << 30,
	}, registry, logical)
	require.NoError(t, err)
	require.NoError(t, applyMQTTMemoryProfile(inputs.RuntimeConfig, deployinfra.BootstrapConfig{
		ContainerMemoryBytes: 1 << 30,
	}))
	return inputs.RuntimeConfig
}

func mqttProfileSessionConfig(t *testing.T, cfg *ports.BridgeConfig, sessionID string) *paho.Config {
	t.Helper()
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID != sessionID {
			continue
		}
		session, ok := cfg.Sessions[i].Config.(*paho.Config)
		require.True(t, ok)
		return session
	}
	t.Fatalf("session %q not found", sessionID)
	return nil
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
