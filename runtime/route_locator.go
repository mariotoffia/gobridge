package runtime

import (
	"context"
	"sync"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// routeLocator implements ports.RouteLocator by combining lease ownership
// with the route-to-session mapping to determine which instance handles a route.
type routeLocator struct {
	instanceID string
	leaseStore ports.LeaseStore

	mu              sync.RWMutex
	routeSessionMap map[string]string // routeID → sessionID (exclusive routes only)
}

func newRouteLocator(instanceID string, leaseStore ports.LeaseStore) *routeLocator {
	return &routeLocator{
		instanceID:      instanceID,
		leaseStore:      leaseStore,
		routeSessionMap: make(map[string]string),
	}
}

// RegisterRoute records that a route uses an exclusive session, enabling
// cluster-aware routing for that route.
func (rl *routeLocator) RegisterRoute(routeID, sessionID string) {
	rl.mu.Lock()
	rl.routeSessionMap[routeID] = sessionID
	rl.mu.Unlock()
}

// Locate determines if a route should be handled locally or forwarded.
// Non-exclusive routes always return local=true.
// Exclusive routes check the lease owner and return PeerInfo if remote.
func (rl *routeLocator) Locate(ctx context.Context, routeID string) (*domain.PeerInfo, bool, error) {
	rl.mu.RLock()
	sessionID, isExclusive := rl.routeSessionMap[routeID]
	rl.mu.RUnlock()

	if !isExclusive || rl.leaseStore == nil {
		return nil, true, nil
	}

	info, err := rl.leaseStore.Current(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}

	if info.Owner == rl.instanceID {
		return nil, true, nil
	}

	return &domain.PeerInfo{
		InstanceID: info.Owner,
		Endpoints:  info.Endpoints,
	}, false, nil
}
