package paho

import (
	"math"
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestConfigIngressMemory_DefaultsNormalize(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, uint32(256<<10), cfg.Session.MaxPayloadBytes)
	assert.Equal(t, uint16(192), cfg.Session.ReceiveMaximum)
	assert.Equal(t, uint64(256<<20), cfg.Session.IngressMemoryBudgetBytes)

	session := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	dispatchDepth, dispatchCapacity := session.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Equal(t, int(DefaultReceiveMaximum), dispatchCapacity)
	assert.Equal(t, DefaultMaxPayloadBytes, session.opts.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, session.opts.IngressMemoryBudgetBytes)
}

func TestConfigIngressMemory_ExplicitZeroNormalizesToDefaults(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"receive_maximum":             0,
			"max_payload_bytes":           0,
			"ingress_memory_budget_bytes": 0,
		},
	})

	assert.Equal(t, DefaultReceiveMaximum, cfg.Session.ReceiveMaximum)
	assert.Equal(t, DefaultMaxPayloadBytes, cfg.Session.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, cfg.Session.IngressMemoryBudgetBytes)
}

func TestConfigIngressMemory_ExactBoundaryAcceptsAndOneByteExcessRejects(t *testing.T) {
	const routeMaxInFlight uint64 = 100
	bound, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, routeMaxInFlight)
	require.NoError(t, err)

	cfg := Config{Session: SessionOptions{
		MaxPayloadBytes:          DefaultMaxPayloadBytes,
		ReceiveMaximum:           DefaultReceiveMaximum,
		IngressMemoryBudgetBytes: bound,
	}}
	require.NoError(t, cfg.ValidateIngressMemory(routeMaxInFlight))

	cfg.Session.IngressMemoryBudgetBytes = bound - 1
	err = cfg.ValidateIngressMemory(routeMaxInFlight)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_RouteConcurrencyContributesToBound(t *testing.T) {
	withoutRoute, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, 0)
	require.NoError(t, err)
	withRoute, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, 37)
	require.NoError(t, err)
	packet, err := ingressMemoryPacketBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	assert.Equal(t, packet*37, withRoute-withoutRoute)
}

func TestConfigIngressMemory_TooSmallForOnePacketRejects(t *testing.T) {
	packet, err := ingressMemoryPacketBytes(DefaultMaxPayloadBytes)
	require.NoError(t, err)

	_, err = LargestSafeReceiveMaximum(DefaultMaxPayloadBytes, packet-1, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestConfigIngressMemory_MaxIntegerOverflowRejects(t *testing.T) {
	tests := []struct {
		name             string
		routeMaxInFlight uint64
	}{
		{name: "window addition", routeMaxInFlight: math.MaxUint64},
		{name: "bound multiplication", routeMaxInFlight: math.MaxUint64 / 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := IngressMemoryBound(DefaultMaxPayloadBytes, math.MaxUint16, test.routeMaxInFlight)
			require.Error(t, err)
			assert.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}
}

func TestRouterIngressMemory_OversizePayloadRejectsBeforeCopyOrEnqueue(t *testing.T) {
	session := NewSession(SessionOptions{
		MaxPayloadBytes: 4,
		ReceiveMaximum:  2,
	}, connectivity.SessionEphemeral, nil)
	session.router.beginGrace()

	handled, err := session.router.onPublishReceived(pahov5.PublishReceived{
		Packet: &pahov5.Publish{Topic: "memory/oversize", QoS: 1, Payload: []byte("12345")},
	})
	require.NoError(t, err)
	assert.True(t, handled)

	dispatchDepth, dispatchCapacity := session.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Equal(t, 2, dispatchCapacity)
	assert.Zero(t, session.router.PendingCount())
	assert.Equal(t, 1, session.Health(t.Context()).UnsettledCount,
		"oversize QoS 1 remains protocol-unsettled so at-least-once is not weakened")
}

func TestConfigIngressMemory_ExplicitUnsafePacketSizeRejectsWithoutClamp(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		MaxPayloadBytes:          math.MaxUint32,
		ReceiveMaximum:           1,
		IngressMemoryBudgetBytes: math.MaxUint64,
	}}

	err := cfg.ValidateIngressMemory(0)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}
