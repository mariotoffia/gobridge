package bridge

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type recordingIngressMemoryConfig struct {
	routeMaxInFlight []uint64
	err              error
}

func (*recordingIngressMemoryConfig) Kind() string    { return "test.ingress-memory" }
func (*recordingIngressMemoryConfig) Validate() error { return nil }
func (c *recordingIngressMemoryConfig) ValidateIngressMemory(routeMaxInFlight uint64) error {
	c.routeMaxInFlight = append(c.routeMaxInFlight, routeMaxInFlight)
	return c.err
}

type dedicatedIngressMemoryFactory struct{ fakeTransportFactory }

func (*dedicatedIngressMemoryFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapDedicatedIngressSession}
}

type ingressOnlyFreezableConfig struct {
	err error
}

func (*ingressOnlyFreezableConfig) Kind() string    { return "test.ingress-only" }
func (*ingressOnlyFreezableConfig) Validate() error { return nil }
func (c *ingressOnlyFreezableConfig) FreezePluginConfig() ports.PluginConfig {
	copy := *c
	return &copy
}
func (c *ingressOnlyFreezableConfig) ValidateIngressMemory(uint64) error { return c.err }

type ingressCapabilityDroppingConfig struct{}

func (*ingressCapabilityDroppingConfig) Kind() string    { return "test.ingress-drop" }
func (*ingressCapabilityDroppingConfig) Validate() error { return nil }
func (*ingressCapabilityDroppingConfig) ValidateIngressMemory(uint64) error {
	return shared.ErrInvalidConfig
}
func (*ingressCapabilityDroppingConfig) FreezePluginConfig() ports.PluginConfig {
	return &freezableWithoutIngressCapability{}
}

type freezableWithoutIngressCapability struct{}

func (*freezableWithoutIngressCapability) Kind() string    { return "test.ingress-drop" }
func (*freezableWithoutIngressCapability) Validate() error { return nil }
func (c *freezableWithoutIngressCapability) FreezePluginConfig() ports.PluginConfig {
	copy := *c
	return &copy
}

func TestIngressMemory_AllIngressSessionsValidatedIndependently(t *testing.T) {
	first := &recordingIngressMemoryConfig{}
	second := &recordingIngressMemoryConfig{}
	senderOnly := &recordingIngressMemoryConfig{}
	cfg := ingressMemoryBridgeConfig(first, second, senderOnly)

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []uint64{routing.DefaultMaxInFlight}, first.routeMaxInFlight)
	assert.Equal(t, []uint64{37}, second.routeMaxInFlight)
	assert.Empty(t, senderOnly.routeMaxInFlight, "sender-only sessions consume no ingress budget")
}

func TestIngressMemory_UnconsumedReceiverStillRunsFullPreflight(t *testing.T) {
	memoryCfg := &recordingIngressMemoryConfig{}
	cfg := ingressMemoryBridgeConfig(memoryCfg, nil, nil)
	cfg.Routes = nil

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []uint64{0}, memoryCfg.routeMaxInFlight)
}

func TestIngressMemory_ReferencedDurableSenderOnlySessionRunsFullPreflight(t *testing.T) {
	memoryCfg := &recordingIngressMemoryConfig{}
	cfg := ingressMemoryBridgeConfig(nil, nil, memoryCfg)
	cfg.Sessions[len(cfg.Sessions)-1].SessionMode = string(connectivity.SessionPersistent)

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []uint64{0}, memoryCfg.routeMaxInFlight)
}

func TestIngressMemory_ValidationFailureIsTypedAndPrecedesResourceCreation(t *testing.T) {
	wantErr := shared.ErrInvalidConfig.WithMessage("unsafe ingress memory")
	memoryCfg := &recordingIngressMemoryConfig{err: wantErr}
	cfg := ingressMemoryBridgeConfig(memoryCfg, nil, nil)
	factory := &countingTransportFactory{}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", factory).
		prepare(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	sessions, receivers, senders := factory.Counts()
	assert.Zero(t, sessions)
	assert.Zero(t, receivers)
	assert.Zero(t, senders)
}

func TestIngressMemory_MaxRouteConcurrencyDoesNotOverflow(t *testing.T) {
	memoryCfg := &recordingIngressMemoryConfig{err: shared.ErrInvalidConfig}
	cfg := ingressMemoryBridgeConfig(memoryCfg, nil, nil)
	cfg.Routes[0].Policy.MaxInFlight = math.MaxInt

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.Equal(t, []uint64{uint64(math.MaxInt)}, memoryCfg.routeMaxInFlight)
}

func TestIngressMemory_Task7ConflictRejectsBeforeMemoryAccounting(t *testing.T) {
	memoryCfg := &recordingIngressMemoryConfig{}
	cfg := ingressMemoryBridgeConfig(memoryCfg, nil, nil)
	cfg.Routes = append(cfg.Routes, ports.RouteDef{
		ID:         "route-conflict",
		ReceiverID: "receiver-a",
		Policy:     ports.PolicyDef{MaxInFlight: 9},
	})

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidConfig))
	assert.Empty(t, memoryCfg.routeMaxInFlight,
		"routes rejected by dedicated-ingress preflight must not be double-counted")
}

func TestIngressMemory_IngressOnlyFreezableConfigReachesPreflight(t *testing.T) {
	memoryCfg := &ingressOnlyFreezableConfig{
		err: shared.ErrInvalidConfig.WithMessage("unsafe ingress-only bound"),
	}
	cfg := ingressMemoryBridgeConfig(memoryCfg, nil, nil)

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestIngressMemory_FreezeRejectsCapabilityLossWithoutActivationTiming(t *testing.T) {
	cfg := ingressMemoryBridgeConfig(&ingressCapabilityDroppingConfig{}, nil, nil)

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("memory-aware", &dedicatedIngressMemoryFactory{}).
		prepare(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func ingressMemoryBridgeConfig(
	first, second, senderOnly ports.PluginConfig,
) *ports.BridgeConfig {
	sessions := []ports.SessionDef{
		{ID: "session-a", Transport: "memory-aware", Config: first},
	}
	receivers := []ports.ReceiverDef{
		{ID: "receiver-a", Transport: "memory-aware", SessionID: "session-a"},
	}
	routes := []ports.RouteDef{
		{ID: "route-a", ReceiverID: "receiver-a"},
	}
	if second != nil {
		sessions = append(sessions, ports.SessionDef{ID: "session-b", Transport: "memory-aware", Config: second})
		receivers = append(receivers, ports.ReceiverDef{ID: "receiver-b", Transport: "memory-aware", SessionID: "session-b"})
		routes = append(routes, ports.RouteDef{
			ID: "route-b", ReceiverID: "receiver-b", Policy: ports.PolicyDef{MaxInFlight: 37},
		})
	}
	var senders []ports.SenderDef
	if senderOnly != nil {
		sessions = append(sessions, ports.SessionDef{ID: "session-sender", Transport: "memory-aware", Config: senderOnly})
		senders = append(senders, ports.SenderDef{
			ID: "sender-only", Transport: "memory-aware", SessionID: "session-sender",
		})
	}
	return &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "ingress-memory"},
		Sessions:  sessions,
		Receivers: receivers,
		Senders:   senders,
		Routes:    routes,
	}
}
