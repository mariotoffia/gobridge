package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// TestLocator_UnregisterRoute_TreatsRouteAsNonExclusive pins that a route
// removed from a running bridge stops being exclusivity-sensitive: Locate
// answers local without consulting the lease store, exactly as for a route that
// was never registered, so a stale mapping cannot send its traffic to the
// owner of a session the route no longer uses.
func TestLocator_UnregisterRoute_TreatsRouteAsNonExclusive(t *testing.T) {
	store := &stubLeaseStore{}
	rl := NewLocator("instance-local", store, LocatorConfig{},
		clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	rl.RegisterRoute("route-1", "sess-1")

	rl.UnregisterRoute("route-1")

	peer, local, err := rl.Locate(context.Background(), "route-1")
	require.NoError(t, err)
	assert.True(t, local, "an unregistered route is handled locally")
	assert.Nil(t, peer)
	assert.Zero(t, store.callCount(), "an unregistered route must not consult the lease store")
}
