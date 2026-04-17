package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteLocatorConfig configures the cluster-aware route locator.
type RouteLocatorConfig struct {
	CacheTTL       time.Duration
	MaxFailures    int
	CooldownPeriod time.Duration
}

// DefaultRouteLocatorConfig returns a RouteLocatorConfig with recommended defaults.
func DefaultRouteLocatorConfig() RouteLocatorConfig {
	return RouteLocatorConfig{
		CacheTTL:       2 * time.Second,
		MaxFailures:    3,
		CooldownPeriod: 5 * time.Second,
	}
}

type cachedLease struct {
	info      domain.LeaseInfo
	fetchedAt time.Time
}

// routeLocator implements ports.RouteLocator by combining lease ownership
// with the route-to-session mapping to determine which instance handles a route.
type routeLocator struct {
	instanceID     string
	leaseStore     ports.LeaseStore
	cacheTTL       time.Duration
	maxFailures    int
	cooldownPeriod time.Duration
	clk            clock.Clock

	mu              sync.RWMutex
	routeSessionMap map[string]string // routeID → sessionID (exclusive routes only)
	cache           map[string]cachedLease

	failMu              sync.Mutex
	consecutiveFailures int
	lastFailure         time.Time
}

func newRouteLocator(instanceID string, leaseStore ports.LeaseStore, cfg RouteLocatorConfig, clk clock.Clock) *routeLocator {
	defaults := DefaultRouteLocatorConfig()
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = defaults.CacheTTL
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = defaults.MaxFailures
	}
	if cfg.CooldownPeriod <= 0 {
		cfg.CooldownPeriod = defaults.CooldownPeriod
	}
	if clk == nil {
		clk = clock.System
	}
	return &routeLocator{
		instanceID:      instanceID,
		leaseStore:      leaseStore,
		cacheTTL:        cfg.CacheTTL,
		maxFailures:     cfg.MaxFailures,
		cooldownPeriod:  cfg.CooldownPeriod,
		clk:             clk,
		routeSessionMap: make(map[string]string),
		cache:           make(map[string]cachedLease),
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

	now := rl.clk.Now()

	rl.mu.RLock()
	cached, hasCached := rl.cache[sessionID]
	rl.mu.RUnlock()

	if hasCached && now.Sub(cached.fetchedAt) < rl.cacheTTL {
		if cached.info.Owner == rl.instanceID {
			return nil, true, nil
		}
		return &domain.PeerInfo{
			InstanceID: cached.info.Owner,
			Endpoints:  cached.info.Endpoints,
		}, false, nil
	}

	if rl.isCircuitOpen(now) {
		return nil, true, nil
	}

	info, err := rl.leaseStore.Current(ctx, sessionID)
	if err != nil {
		rl.recordFailure(now)
		if hasCached {
			if cached.info.Owner == rl.instanceID {
				return nil, true, nil
			}
			return &domain.PeerInfo{
				InstanceID: cached.info.Owner,
				Endpoints:  cached.info.Endpoints,
			}, false, nil
		}
		return nil, false, err
	}

	rl.recordSuccess()

	rl.mu.Lock()
	rl.cache[sessionID] = cachedLease{info: info, fetchedAt: now}
	rl.mu.Unlock()

	if info.Owner == rl.instanceID {
		return nil, true, nil
	}

	return &domain.PeerInfo{
		InstanceID: info.Owner,
		Endpoints:  info.Endpoints,
	}, false, nil
}

func (rl *routeLocator) isCircuitOpen(now time.Time) bool {
	rl.failMu.Lock()
	defer rl.failMu.Unlock()
	if rl.consecutiveFailures < rl.maxFailures {
		return false
	}
	return now.Sub(rl.lastFailure) < rl.cooldownPeriod
}

func (rl *routeLocator) recordFailure(now time.Time) {
	rl.failMu.Lock()
	rl.consecutiveFailures++
	rl.lastFailure = now
	rl.failMu.Unlock()
}

func (rl *routeLocator) recordSuccess() {
	rl.failMu.Lock()
	rl.consecutiveFailures = 0
	rl.failMu.Unlock()
}
