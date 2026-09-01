package ports_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/ports"
)

// TestReadinessLevelFromDeepHealth_EmptyRuntimeIsNotFull proves an instance
// that carries NO routes and NO sessions cannot advertise LevelFull. LevelFull
// means "every route is ready to dispatch"; over an empty set that is
// vacuously true, so a bridge that started with a missing config file — zero
// routes, nothing bridged — would otherwise pass a pre-traffic gate while no
// message can ever flow through it.
func TestReadinessLevelFromDeepHealth_EmptyRuntimeIsNotFull(t *testing.T) {
	dh := ports.DeepHealth{Running: true, Healthy: true, Empty: true}

	assert.Equal(t, ports.LevelRunning, ports.ReadinessLevelFromDeepHealth(dh),
		"an instance bridging nothing is running, but it is not connected, subscribed or full")
}

// TestReadinessLevelFromDeepHealth_ConfiguredRuntimeStillReachesFull proves the
// empty cap is keyed on the explicit Empty projection and nothing else: a
// configured instance whose sessions and routes are all ready still reports
// Full.
func TestReadinessLevelFromDeepHealth_ConfiguredRuntimeStillReachesFull(t *testing.T) {
	satisfied := true
	dh := ports.DeepHealth{
		Running: true,
		Healthy: true,
		Sessions: []ports.SessionHealthDetail{{
			SessionID:              "sess-1",
			Connected:              true,
			SubscriptionsSatisfied: &satisfied,
			ServiceLevel:           ports.ServiceLevelFull,
			Ready:                  true,
		}},
		Routes:          []ports.RouteHealth{{ID: "route-1", Ready: true}},
		ReadyForTraffic: true,
	}

	assert.Equal(t, ports.LevelFull, ports.ReadinessLevelFromDeepHealth(dh))
}
