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

	assert.Zero(t, cfg.Session.MaxPayloadBytes)
	assert.Zero(t, cfg.Session.ReceiveMaximum)
	assert.Zero(t, cfg.Session.IngressMemoryBudgetBytes,
		"parsed defaults stay unset until deployment preflight/runtime normalization")

	session := NewSession(SessionOptions{}, connectivity.SessionEphemeral, nil)
	dispatchDepth, dispatchCapacity := session.IngressMemoryStats()
	assert.Zero(t, dispatchDepth)
	assert.Equal(t, int(DefaultReceiveMaximum), dispatchCapacity)
	assert.Equal(t, DefaultMaxPayloadBytes, session.opts.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, session.opts.IngressMemoryBudgetBytes)
}

func TestConfigIngressMemory_ExplicitZeroRemainsUnsetUntilRuntimeNormalization(t *testing.T) {
	cfg := decodeRegistry(t, map[string]any{
		"session": map[string]any{
			"receive_maximum":             0,
			"max_payload_bytes":           0,
			"ingress_memory_budget_bytes": 0,
		},
	})

	assert.Zero(t, cfg.Session.ReceiveMaximum)
	assert.Zero(t, cfg.Session.MaxPayloadBytes)
	assert.Zero(t, cfg.Session.IngressMemoryBudgetBytes)

	session := NewSession(cfg.Session, connectivity.SessionEphemeral, nil)
	assert.Equal(t, DefaultReceiveMaximum, session.opts.ReceiveMaximum)
	assert.Equal(t, DefaultMaxPayloadBytes, session.opts.MaxPayloadBytes)
	assert.Equal(t, DefaultIngressMemoryBudgetBytes, session.opts.IngressMemoryBudgetBytes)
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

func TestConfigIngressMemory_DirectMapIntegerBoundaries(t *testing.T) {
	twoTo64 := math.Ldexp(1, 64)
	nextBelowTwoTo64 := math.Nextafter(twoTo64, 0)
	tests := []struct {
		name      string
		key       string
		value     any
		wantErr   bool
		wantValue uint64
	}{
		{
			name: "receive maximum exact max",
			key:  "receive_maximum", value: uint64(math.MaxUint16),
			wantValue: math.MaxUint16,
		},
		{
			name: "receive maximum max plus one",
			key:  "receive_maximum", value: uint64(math.MaxUint16) + 1,
			wantErr: true,
		},
		{
			name: "payload exact uint32 max",
			key:  "max_payload_bytes", value: uint64(math.MaxUint32),
			wantValue: math.MaxUint32,
		},
		{
			name: "payload uint32 max plus one",
			key:  "max_payload_bytes", value: uint64(math.MaxUint32) + 1,
			wantErr: true,
		},
		{
			name: "ingress budget exact uint64 max",
			key:  "ingress_memory_budget_bytes", value: uint64(math.MaxUint64),
			wantValue: math.MaxUint64,
		},
		{
			name: "float nextafter below two to 64 accepted for uint64",
			key:  "ingress_memory_budget_bytes", value: nextBelowTwoTo64,
			wantValue: uint64(nextBelowTwoTo64),
		},
		{
			name: "float nextafter below two to 64 rejected for uint16",
			key:  "receive_maximum", value: nextBelowTwoTo64,
			wantErr: true,
		},
		{
			name: "float exactly two to 64 rejected",
			key:  "ingress_memory_budget_bytes", value: twoTo64,
			wantErr: true,
		},
		{
			name: "negative int64 rejected",
			key:  "ingress_memory_budget_bytes", value: int64(-1),
			wantErr: true,
		},
		{
			name: "nan rejected",
			key:  "ingress_memory_budget_bytes", value: math.NaN(),
			wantErr: true,
		},
		{
			name: "positive infinity rejected",
			key:  "ingress_memory_budget_bytes", value: math.Inf(1),
			wantErr: true,
		},
		{
			name: "negative infinity rejected",
			key:  "ingress_memory_budget_bytes", value: math.Inf(-1),
			wantErr: true,
		},
		{
			name: "fraction rejected",
			key:  "ingress_memory_budget_bytes", value: 1.5,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := SessionOptionsFromMap(map[string]any{test.key: test.value})
			if test.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, shared.ErrInvalidConfig)
				return
			}
			require.NoError(t, err)
			var value uint64
			switch test.key {
			case "receive_maximum":
				value = uint64(options.ReceiveMaximum)
			case "max_payload_bytes":
				value = uint64(options.MaxPayloadBytes)
			case "ingress_memory_budget_bytes":
				value = options.IngressMemoryBudgetBytes
			}
			assert.Equal(t, test.wantValue, value)
		})
	}
}
