package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// LocatorConfig configures the cluster-aware route locator.
type LocatorConfig struct {
	CacheTTL       time.Duration
	MaxFailures    int
	CooldownPeriod time.Duration

	// FailOpen selects the posture used when the current owner of an
	// exclusivity-sensitive route cannot be determined — the lease store is
	// failing (breaker open) or a transient error leaves no cached owner.
	//
	// Default (false = fail-CLOSED, consistent with the session layer): refuse
	// the routing decision by returning an error, so a non-owner never processes
	// or forwards an exclusive route while ownership is unverifiable. This trades
	// availability for strict single-owner exclusivity.
	//
	// Setting FailOpen = true restores optimistic LOCAL processing during those
	// windows, trading exclusivity for availability. Only enable it where the
	// workload tolerates transient duplicate processing (fencing on the data
	// path still prevents duplicate commits).
	FailOpen bool

	// Metrics receives shared.MetricRouteOwnerUnknown, the reason-tagged
	// disclosure of every decision taken without a verifiable owner. Nil is
	// replaced by a no-op exporter.
	Metrics ports.MetricsExporter
}

// Reasons carried on shared.MetricRouteOwnerUnknown. They are the operator's
// only way to separate fleet clock skew and a cold-fleet takeover window —
// both of which surface as reasonLeaseExpired — from a failing lease store.
const (
	// reasonLeaseExpired: the local clock is at or past the owner-written
	// ExpiresAt. The owner may still consider the lease live — expiry is read
	// off THIS node's wall clock, so skew above the renew margin lands here.
	reasonLeaseExpired = "lease_expired"
	// reasonLeaseUnowned: no lease row exists (normal mid-transfer window).
	reasonLeaseUnowned = "lease_unowned"
	// reasonStoreUnavailable: the lease store failed and no usable cached owner
	// remains.
	reasonStoreUnavailable = "store_unavailable"
	// reasonStoreBreakerOpen: the breaker is open, so the decision was refused
	// without calling the repeatedly-failing store.
	reasonStoreBreakerOpen = "store_breaker_open"
)

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
	failOpen       bool
	clk            clock.Clock
	metrics        ports.MetricsExporter

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
	if cfg.Metrics == nil {
		cfg.Metrics = &ports.NoopExporter{}
	}
	return &Locator{
		instanceID:      instanceID,
		leaseStore:      leaseStore,
		cacheTTL:        cfg.CacheTTL,
		maxFailures:     cfg.MaxFailures,
		cooldownPeriod:  cfg.CooldownPeriod,
		failOpen:        cfg.FailOpen,
		clk:             clk,
		metrics:         cfg.Metrics,
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

	// Serve the cached owner only while the entry is BOTH fresh (within CacheTTL)
	// AND still unexpired. The ExpiresAt bound mirrors the fresh-read and
	// stale-fallback paths below: a lease can expire inside its CacheTTL window
	// (nothing pins CacheTTL below lease_ttl), and serving an expired cached owner
	// would forward exclusive traffic to a corpse for the rest of the CacheTTL —
	// the same hazard the fresh-read bound closes. Past expiry we
	// fall through to a fresh read.
	if hasCached && now.Sub(cached.fetchedAt) < rl.cacheTTL && now.Before(cached.info.ExpiresAt) {
		if cached.info.Owner == rl.instanceID {
			return nil, true, nil
		}
		return &persistence.PeerInfo{
			InstanceID: cached.info.Owner,
			Endpoints:  cached.info.Endpoints,
		}, false, nil
	}

	if rl.isCircuitOpen(now) {
		// The lease store has been failing repeatedly. We cannot verify the
		// current owner, so apply the configured posture rather than blindly
		// processing locally. Default is fail-CLOSED (consistent with the
		// session layer): refuse the decision so a non-owner does not process an
		// exclusive route during a store outage.
		return rl.onOwnershipUnknown(shared.ErrUnavailable, reasonStoreBreakerOpen)
	}

	info, err := rl.leaseStore.Current(ctx, sessionID)
	if err != nil {
		// A not-found lease is NORMAL: the lease is momentarily unowned (mid
		// transfer, or before the first acquisition). It is NOT a store failure
		// and must NOT count toward the breaker, otherwise the breaker opens on
		// every ordinary lease transfer. We still cannot name an
		// owner, so apply the ownership-unknown posture.
		if errors.Is(err, shared.ErrNotFound) {
			return rl.onOwnershipUnknown(shared.ErrNoRouteOwner, reasonLeaseUnowned)
		}

		rl.recordFailure(now)
		if hasCached && now.Before(cached.info.ExpiresAt) {
			// A transient store error with a last-known owner that has NOT yet
			// passed its own lease expiry: serve the cached owner rather than
			// fail, so a brief blip does not disrupt routing. Fencing on the data
			// path still rejects a stale forward.
			//
			// The ExpiresAt bound is essential: a store outage can outlast the
			// lease, after which the cached owner may have stepped down and a new
			// owner (or none) taken over. Serving an age-unbounded stale owner
			// then forwards exclusive traffic to an instance that no longer holds
			// the lease indefinitely. Past expiry we fall through
			// to the ownership-unknown posture instead.
			if cached.info.Owner == rl.instanceID {
				return nil, true, nil
			}
			return &persistence.PeerInfo{
				InstanceID: cached.info.Owner,
				Endpoints:  cached.info.Endpoints,
			}, false, nil
		}
		return rl.onOwnershipUnknown(err, reasonStoreUnavailable)
	}

	rl.recordSuccess()

	if !now.Before(info.ExpiresAt) {
		// The freshly-read lease row has already passed its own expiry but has
		// not yet been seized by a standby: the named owner is a corpse. Serving
		// it as authoritative forwards exclusive traffic to a dead owner for up
		// to TTL+observation. Apply the same ExpiresAt bound the cached
		// stale-fallback path enforces above and fall back to the
		// ownership-unknown posture (503 + Retry-After) instead.
		//
		// The corpse is intentionally NOT cached: caching an already-expired row
		// is pointless — the cache-hit path above now re-checks ExpiresAt and
		// would skip it anyway — and leaving the cache untouched forces the next
		// call to re-read until a live owner (or none) is observed.
		return rl.onOwnershipUnknown(shared.ErrNoRouteOwner, reasonLeaseExpired)
	}

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

// onOwnershipUnknown applies the configured posture when the locator cannot
// determine the current owner of an exclusivity-sensitive route (breaker open,
// unowned lease, or a transient store error with no cached owner).
//
// Default (fail-CLOSED, consistent with the session layer): return err so the
// caller refuses to process or forward the route while ownership is
// unverifiable — a non-owner must not act on an exclusive route. Callers surface
// this as an error (e.g. HTTP 502), which is the safe outcome for exclusivity.
//
// LocatorConfig.FailOpen switches to optimistic LOCAL processing (local=true,
// no error), trading exclusivity for availability where the workload tolerates
// transient duplicate processing.
//
// Every call emits shared.MetricRouteOwnerUnknown tagged with reason, so an
// operator can attribute the 502/503 (or the fail-open duplicate risk) to clock
// skew, a normal transfer window, or a failing lease store.
func (rl *Locator) onOwnershipUnknown(err error, reason string) (*persistence.PeerInfo, bool, error) {
	// Disclose the decision BEFORE the posture branch: under FailOpen the route
	// is processed locally with no error, so this counter is the only trace that
	// ownership was unverifiable.
	rl.metrics.Counter(shared.MetricRouteOwnerUnknown, 1,
		shared.Tag{Key: shared.TagKeyReason, Value: reason})
	if rl.failOpen {
		return nil, true, nil
	}
	return nil, false, err
}

// Compile-time port-conformance assertion.
var _ ports.RouteLocator = (*Locator)(nil)
