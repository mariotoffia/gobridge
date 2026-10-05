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

func TestPreflight_NegativeRouteMaxInFlightRefused(t *testing.T) {
	cfg := routeMaxInFlightConfig(-1)

	err := bridge.NewBuilder(cfg).Preflight(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
	var bridgeErr *shared.BridgeError
	require.True(t, errors.As(err, &bridgeErr))
	assert.Equal(t, shared.ErrInvalidConfig.Code, bridgeErr.Code)
	assert.Equal(t, shared.ErrInvalidConfig.Class, bridgeErr.Class)
	assert.Equal(t, `bridge: route "route-a": policy.max_in_flight must not be negative`, bridgeErr.Message)
}

func TestPreflight_ZeroOrPositiveRouteMaxInFlightAccepted(t *testing.T) {
	for _, maxInFlight := range []int{0, 1, 37} {
		t.Run(fmt.Sprintf("max_in_flight=%d", maxInFlight), func(t *testing.T) {
			cfg := routeMaxInFlightConfig(maxInFlight)
			require.NoError(t, bridge.NewBuilder(cfg).Preflight(t.Context()))
		})
	}
}

func routeMaxInFlightConfig(maxInFlight int) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "route-max-in-flight"},
		Receivers: []ports.ReceiverDef{{ID: "receiver-a", Transport: "test"}},
		Routes: []ports.RouteDef{{
			ID:         "route-a",
			ReceiverID: "receiver-a",
			Policy:     ports.PolicyDef{MaxInFlight: maxInFlight},
		}},
	}
}
