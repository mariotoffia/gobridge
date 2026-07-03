package circuitbreaker

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Processor = (*Processor)(nil)

// Processor is a circuit breaker that tracks failures per key and short-circuits
// requests when a breaker trips open.
//
// It gates only the downstream processors that run after it in the chain
// (its next); it does not observe the sender, which the runtime invokes
// after the chain returns. When open it returns shared.ErrUnavailable
// (transient) so the runtime retries with backoff rather than invoking the
// failing downstream. See the package doc for the scope boundary.
type Processor struct {
	name          string
	config        cb.Config
	keyExtractor  KeyExtractor
	onStateChange func(key string, from, to cb.State)
	logger        *slog.Logger
	metrics       ports.MetricsExporter
	maxBreakers   int
	clk           clock.Clock
	mu            sync.Mutex
	breakers      map[string]*breakerEntry
	// lru orders breakerEntry values by recency of use (front = most
	// recent). Maintained under p.mu with O(1) updates so eviction never
	// scans the whole cache (see evictLocked).
	lru            *list.List
	totalEvictions int64
	openEvictions  int64
}

// breakerEntry pairs a cached breaker with a count of the Process calls
// currently mid-flight on it, plus its key and LRU list node so eviction
// and recency updates are O(1).
//
// inFlight is the cache-lifecycle pin that closes a pre-existing orphan window:
// Process fetches a breaker under p.mu, then uses it (BeforeRequestToken, the
// downstream call, AfterRequestToken) OUTSIDE the lock so the hot path never
// serializes on p.mu. Without a pin, a concurrent evictLocked could delete the
// very breaker an in-flight goroutine still had to update, so its outcome
// landed on an orphan while a freshly created breaker took the key -- a silent
// lost state update (a failure that should have tripped the key open).
//
// The pin is taken under p.mu when Process adopts the breaker and released
// (atomically, lock-free) only AFTER AfterRequestToken has recorded the
// outcome. evictLocked skips any entry with inFlight > 0, so a breaker is
// dropped only once it is truly idle. The increment happens-before p.mu is
// released and evictLocked reads inFlight under p.mu, so an adopted breaker is
// always seen as pinned; the lock-free decrement keeps the extra bookkeeping
// off the hot path. breaker is assigned once at creation and never reassigned,
// so reads of it need no lock; key and elem are written once in insertLocked
// under p.mu and only read under p.mu afterwards.
type breakerEntry struct {
	breaker  *cb.Breaker
	key      string
	elem     *list.Element
	inFlight atomic.Int32
}

func New(name string, cfg cb.Config, opts ...Option) *Processor {
	p := &Processor{
		name:         name,
		config:       cfg.WithDefaults(),
		keyExtractor: GlobalKey,
		maxBreakers:  defaultMaxBreakers,
		clk:          clock.System,
		breakers:     make(map[string]*breakerEntry),
		lru:          list.New(),
	}
	for _, o := range opts {
		o(p)
	}
	if p.maxBreakers <= 0 {
		p.maxBreakers = defaultMaxBreakers
	}
	if p.clk == nil {
		p.clk = clock.System
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.metrics == nil {
		p.metrics = &ports.NoopExporter{}
	}
	if p.onStateChange == nil {
		// Default observability: a breaker state transition is an
		// operational event (a downstream started or stopped failing) and
		// must never be silent. Overridable via WithOnStateChange.
		logger := p.logger
		name := p.Name()
		p.onStateChange = func(key string, from, to cb.State) {
			logger.Warn("circuit breaker state change",
				"processor", name, "key", key,
				"from", from.String(), "to", to.String())
		}
	}
	// Metrics are emitted for every transition regardless of which
	// notification handler (default log or WithOnStateChange) is installed.
	{
		notify := p.onStateChange
		metrics := p.metrics
		name := p.Name()
		p.onStateChange = func(key string, from, to cb.State) {
			metrics.Counter(shared.MetricCircuitBreakerStateChanged, 1,
				shared.Tag{Key: "processor", Value: name},
				shared.Tag{Key: "key", Value: key},
				shared.Tag{Key: "to", Value: to.String()})
			if to == cb.StateOpen {
				metrics.Counter(shared.MetricCircuitBreakerTrips, 1,
					shared.Tag{Key: "processor", Value: name},
					shared.Tag{Key: "key", Value: key})
			}
			notify(key, from, to)
		}
	}
	return p
}

func (p *Processor) Name() string {
	if p.name == "" {
		return "circuitbreaker"
	}
	return p.name
}

// defaultMaxBreakers bounds the per-Processor breaker cache when no
// WithMaxBreakers override is supplied. High-cardinality KeyExtractors over
// untrusted input (for example HeaderKey on a caller-controlled header) can
// churn this cache and, at the extreme, evict open breakers; keep the key
// space bounded and trusted, size the cache with WithMaxBreakers, and watch
// Stats().OpenEvictions.
const defaultMaxBreakers = 10000

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	key := p.keyExtractor(ctx, env)

	p.mu.Lock()
	e, ok := p.breakers[key]
	if !ok {
		if len(p.breakers) >= p.maxBreakers {
			p.evictLocked()
		}
		e = &breakerEntry{breaker: cb.NewBreaker(key, p.config, p.onStateChange, cb.WithBreakerClock(p.clk))}
		p.insertLocked(key, e)
	} else {
		p.lru.MoveToFront(e.elem)
	}
	// Pin the breaker while this call is in flight so evictLocked cannot drop
	// it before the outcome is recorded. Incremented under p.mu so an
	// eviction racing this Process always observes the pin.
	e.inFlight.Add(1)
	p.mu.Unlock()

	b := e.breaker
	// Release the pin only after the outcome is recorded. Registered first,
	// so by LIFO it fires last -- after the AfterRequestToken defer below and
	// after the BeforeRequestToken short-circuit return, on every path
	// including a panic.
	defer e.inFlight.Add(-1)

	// The token pins the breaker generation this request was admitted in;
	// AfterRequestToken discards the outcome if the circuit changed state
	// while the request was in flight (stale evidence must not corrupt
	// half-open probing).
	tok, err := b.BeforeRequestToken()
	if err != nil {
		// Short-circuited: the breaker is open (or the half-open probe
		// slot is taken). Count the rejection so a tripped breaker under
		// sustained traffic is visible without log scraping.
		p.metrics.Counter(shared.MetricCircuitBreakerRejections, 1,
			shared.Tag{Key: "processor", Value: p.Name()},
			shared.Tag{Key: "key", Value: key})
		return err
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("circuitbreaker: panic in processor chain: %v", r)
			b.AfterRequestToken(tok, err)
			panic(r)
		}
		b.AfterRequestToken(tok, err)
	}()

	err = next(ctx, env)
	return err
}

// insertLocked registers a new breaker entry under key in both the map and
// the LRU list. Caller must hold p.mu.
func (p *Processor) insertLocked(key string, e *breakerEntry) {
	e.key = key
	e.elem = p.lru.PushFront(e)
	p.breakers[key] = e
}

// maxEvictionScan bounds how many LRU-tail entries a single eviction
// examines. Eviction cost is O(maxEvictionScan), independent of cache size:
// the previous full-cache scan made every new-key insert on a full cache pay
// an O(maxBreakers) walk that took every breaker's lock while holding p.mu —
// a lock-convoy DoS shape under a high-cardinality key extractor.
const maxEvictionScan = 64

// evictLocked makes room in a full breaker cache by examining at most
// maxEvictionScan entries from the least-recently-used end of the cache. It
// prefers a CLOSED breaker, then a half-open one, and only as a last resort
// an open breaker -- open breakers protect against known-failing
// dependencies, so dropping one is counted separately in
// Stats().OpenEvictions as a churn signal. Called with p.mu held.
//
// Entries with inFlight > 0 are skipped in every tier: a breaker a concurrent
// Process still has to update must never be dropped, or that update would be
// lost on an orphan. If no evictable entry is found within the scan bound,
// no eviction happens and the caller inserts anyway; the cache overflows by a
// bounded, transient amount and self-corrects as in-flight calls drain.
func (p *Processor) evictLocked() {
	var halfOpenVictim, anyVictim *breakerEntry

	examined := 0
	for el := p.lru.Back(); el != nil && examined < maxEvictionScan; el = el.Prev() {
		examined++
		e := el.Value.(*breakerEntry)
		if e.inFlight.Load() > 0 {
			continue
		}
		switch e.breaker.GetMetrics().State {
		case cb.StateClosed.String():
			// Best victim tier: evict immediately.
			p.removeLocked(e)
			return
		case cb.StateHalfOpen.String():
			if halfOpenVictim == nil {
				halfOpenVictim = e
			}
		default:
			if anyVictim == nil {
				anyVictim = e
			}
		}
	}

	if halfOpenVictim != nil {
		p.removeLocked(halfOpenVictim)
		return
	}
	if anyVictim != nil {
		p.removeLocked(anyVictim)
		return
	}
}

// removeLocked drops an entry from the map and LRU list and updates the
// eviction counters. Breaker state is mutated outside p.mu, so the victim's
// state is re-read at drop time: only a genuine open drop increments
// OpenEvictions, keeping that churn red-flag exact. Caller must hold p.mu.
func (p *Processor) removeLocked(e *breakerEntry) {
	delete(p.breakers, e.key)
	p.lru.Remove(e.elem)
	p.totalEvictions++
	if e.breaker.GetMetrics().State == cb.StateOpen.String() {
		p.openEvictions++
	}
}

// Stats is a point-in-time snapshot of the breaker cache for observability.
type Stats struct {
	Capacity      int   // configured maximum number of cached breakers
	Size          int   // breakers currently cached
	Evictions     int64 // total breakers evicted to bound the cache
	OpenEvictions int64 // subset of Evictions that dropped an OPEN breaker
}

// Stats returns cache-pressure counters. A non-zero OpenEvictions means the
// cache is too small for the key cardinality and active protection is being
// dropped -- raise WithMaxBreakers or bound (and trust) the key space.
func (p *Processor) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Capacity:      p.maxBreakers,
		Size:          len(p.breakers),
		Evictions:     p.totalEvictions,
		OpenEvictions: p.openEvictions,
	}
}

func (p *Processor) Metrics() map[string]cb.BreakerMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()

	m := make(map[string]cb.BreakerMetrics, len(p.breakers))
	for k, e := range p.breakers {
		m[k] = e.breaker.GetMetrics()
	}
	return m
}
