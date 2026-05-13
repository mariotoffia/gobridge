package cluster

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// LocatorConfig configures the cluster-aware route locator.
type LocatorConfig struct {
	CacheTTL       time.Duration
	MaxFailures    int
	CooldownPeriod time.Duration
}

// DefaultLocatorConfig returns a LocatorConfig with recommended defaults.
func DefaultLocatorConfig() LocatorConfig {
	return LocatorConfig{
		CacheTTL:       2 * time.Second,
		MaxFailures:    3,
		CooldownPeriod: 5 * time.Second,
	}
}

type cachedLease struct {
	info      persistence.LeaseInfo
	fetchedAt time.Time
}

// Locator implements ports.RouteLocator by combining lease ownership
// with the route-to-session mapping to determine which instance handles
// a given route.
type Locator struct {
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

// NewLocator constructs a cluster-aware route locator. Zero or negative
// config fields fall back to [DefaultLocatorConfig]; a nil clock falls
// back to [clock.System].
func NewLocator(instanceID string, leaseStore ports.LeaseStore, cfg LocatorConfig, clk clock.Clock) *Locator {
	defaults := DefaultLocatorConfig()
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
	return &Locator{
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
func (rl *Locator) RegisterRoute(routeID, sessionID string) {
	rl.mu.Lock()
	rl.routeSessionMap[routeID] = sessionID
	rl.mu.Unlock()
}

// Locate determines if a route should be handled locally or forwarded.
// Non-exclusive routes always return local=true.
// Exclusive routes check the lease owner and return PeerInfo if remote.
func (rl *Locator) Locate(ctx context.Context, routeID string) (*persistence.PeerInfo, bool, error) {
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
		return &persistence.PeerInfo{
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
			return &persistence.PeerInfo{
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

	return &persistence.PeerInfo{
		InstanceID: info.Owner,
		Endpoints:  info.Endpoints,
	}, false, nil
}

func (rl *Locator) isCircuitOpen(now time.Time) bool {
	rl.failMu.Lock()
	defer rl.failMu.Unlock()
	if rl.consecutiveFailures < rl.maxFailures {
		return false
	}
	return now.Sub(rl.lastFailure) < rl.cooldownPeriod
}

func (rl *Locator) recordFailure(now time.Time) {
	rl.failMu.Lock()
	rl.consecutiveFailures++
	rl.lastFailure = now
	rl.failMu.Unlock()
}

func (rl *Locator) recordSuccess() {
	rl.failMu.Lock()
	rl.consecutiveFailures = 0
	rl.failMu.Unlock()
}

// Compile-time port-conformance assertion.
var _ ports.RouteLocator = (*Locator)(nil)
