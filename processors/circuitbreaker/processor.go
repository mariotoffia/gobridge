package circuitbreaker

import (
	"container/list"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"unicode/utf8"

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
	// metricKeyMu guards metricKeys. It is a dedicated leaf lock (never
	// p.mu) so bounding the metric-key dimension never contends with the
	// hot-path breaker map and has no lock-ordering coupling: the
	// onStateChange notifier fires from the breaker OUTSIDE p.mu.
	metricKeyMu sync.Mutex
	// metricKeys is the bounded set of NORMALIZED breaker keys already
	// emitted as the "key" metric dimension. It never grows past
	// metricKeyLimit entries, and each entry is length-capped by
	// normalizeMetricKey, so total memory is bounded by
	// metricKeyLimit * maxMetricKeyLen even under a caller-controlled,
	// high-cardinality, large-value KeyExtractor.
	metricKeys map[string]struct{}
	// metricKeyLimit caps the number of DISTINCT breaker keys tagged
	// verbatim on the shared circuit-breaker metrics; any further key
	// collapses to metricKeyOverflow. See WithMetricKeyCardinality.
	metricKeyLimit int
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
		name:           name,
		config:         cfg.WithDefaults(),
		keyExtractor:   GlobalKey,
		maxBreakers:    defaultMaxBreakers,
		clk:            clock.System,
		breakers:       make(map[string]*breakerEntry),
		lru:            list.New(),
		metricKeys:     make(map[string]struct{}),
		metricKeyLimit: defaultMetricKeyCardinality,
	}
	for _, o := range opts {
		o(p)
	}
	if p.maxBreakers <= 0 {
		p.maxBreakers = defaultMaxBreakers
	}
	if p.metricKeyLimit <= 0 {
		p.metricKeyLimit = defaultMetricKeyCardinality
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
		proc := p
		p.onStateChange = func(key string, from, to cb.State) {
			// Bound the metric "key" dimension: the breaker key can be
			// caller-controlled (HeaderKey over an untrusted header), so
			// tagging it verbatim is an unbounded-cardinality DoS on the
			// metrics backend. The default log/notifier still receives the
			// RAW key (logs are not per-series cardinality-priced).
			mkey := proc.metricKey(key)
			metrics.Counter(shared.MetricCircuitBreakerStateChanged, 1,
				shared.Tag{Key: "processor", Value: name},
				shared.Tag{Key: "key", Value: mkey},
				shared.Tag{Key: "to", Value: to.String()})
			if to == cb.StateOpen {
				metrics.Counter(shared.MetricCircuitBreakerTrips, 1,
					shared.Tag{Key: "processor", Value: name},
					shared.Tag{Key: "key", Value: mkey})
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

// metricKeyOverflow is the RESERVED sentinel "key" metric dimension value,
// emitted once the bounded metric-key set is full (see metricKey): every
// distinct breaker key beyond the configured limit shares this one series,
// hard-capping cardinality. The name is deliberately reserved (double
// underscores) so a collision with a real breaker key is extremely unlikely;
// and a raw key that literally equals it is FOLDED into this same overflow
// bucket, so the sentinel series is never ambiguous -- it always means "keys
// beyond the cap, plus any key named the reserved sentinel". Treat this string
// as reserved: do not emit it from a KeyExtractor as a meaningful key.
const metricKeyOverflow = "__other__"

// maxMetricKeyLen bounds the byte length of any value stored in p.metricKeys
// and emitted as the "key" metric dimension. A caller-controlled KeyExtractor
// (HeaderKey over an untrusted header) can produce arbitrarily long keys;
// without this cap the bounded SET would still retain unbounded BYTES per entry
// and emit giant metric labels. An oversized key is folded to a stable prefix
// plus a short hash (see normalizeMetricKey), so distinct long keys keep
// distinct labels with high probability while total memory stays bounded by
// metricKeyLimit * maxMetricKeyLen.
const maxMetricKeyLen = 128

// defaultMetricKeyCardinality bounds the number of DISTINCT breaker keys
// tagged verbatim on the shared circuit-breaker metrics when no
// WithMetricKeyCardinality override is given. It is independent of (and far
// below) defaultMaxBreakers: the breaker CACHE may legitimately hold many
// keys, but the number of distinct metric SERIES the "key" dimension may
// create must stay bounded so a high-cardinality KeyExtractor over untrusted
// input (HeaderKey on a caller-controlled header) cannot explode the metrics
// backend during the very outage the breaker should make observable.
const defaultMetricKeyCardinality = 256

// normalizeMetricKey caps a raw breaker key to at most maxMetricKeyLen bytes
// for use as a metric label. A short key passes through verbatim; an oversized
// key becomes "<utf8-safe prefix>#<16-hex fnv64a hash of the full key>", which
// is bounded, stable across calls, and distinguishes distinct long keys with
// high probability. The hash is a non-cryptographic bucketing aid, not a
// security control.
func normalizeMetricKey(key string) string {
	if len(key) <= maxMetricKeyLen {
		return key
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	const hexLen = 16                        // %016x of a uint64
	prefix := key[:maxMetricKeyLen-hexLen-1] // leave room for the '#' separator
	// Never split a multi-byte rune at the truncation point: keep the label
	// valid UTF-8 for exporters that require it (at most 3 bytes trimmed).
	for len(prefix) > 0 {
		if r, size := utf8.DecodeLastRuneInString(prefix); r != utf8.RuneError || size > 1 {
			break
		}
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s#%016x", prefix, h.Sum64())
}

// metricKey maps a raw breaker key to a bounded value for the "key" metric
// dimension. The key can be caller-controlled (for example HeaderKey over an
// untrusted header), so tagging it verbatim would let one producer drive both
// unbounded metric cardinality AND unbounded label bytes -- a telemetry cost /
// throttling DoS. Defence is two-layered: normalizeMetricKey caps the label
// LENGTH (bounded bytes per series) and the bounded set caps the DISTINCT COUNT
// (bounded number of series). The first metricKeyLimit distinct normalized keys
// pass through so a trusted, bounded key space (GlobalKey, a handful of
// subjects/tenants) stays fully observable; any further distinct key -- or a
// key that literally equals the reserved overflow sentinel -- collapses to
// metricKeyOverflow, capping the dimension at metricKeyLimit+1 series
// regardless of input. Safe for concurrent use: the notifier fires from
// multiple breaker goroutines and Process rejects concurrently.
func (p *Processor) metricKey(key string) string {
	label := normalizeMetricKey(key)
	// A raw key that equals the reserved sentinel is folded into the overflow
	// bucket so that series is never ambiguous; it is neither stored nor
	// counted against the limit.
	if label == metricKeyOverflow {
		return metricKeyOverflow
	}
	p.metricKeyMu.Lock()
	defer p.metricKeyMu.Unlock()
	if _, ok := p.metricKeys[label]; ok {
		return label
	}
	if len(p.metricKeys) >= p.metricKeyLimit {
		return metricKeyOverflow
	}
	p.metricKeys[label] = struct{}{}
	return label
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
			shared.Tag{Key: "key", Value: p.metricKey(key)})
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
	Capacity      int   // configured SOFT cap on cached breakers (Size may briefly exceed it, see Stats)
	Size          int   // breakers currently cached
	Evictions     int64 // total breakers evicted to bound the cache
	OpenEvictions int64 // subset of Evictions that dropped an OPEN breaker
}

// Stats returns cache-pressure counters. A non-zero OpenEvictions means the
// cache is too small for the key cardinality and active protection is being
// dropped -- raise WithMaxBreakers or bound (and trust) the key space.
//
// Capacity is a SOFT cap, not a hard memory bound: when the cache is full and
// every entry examined by evictLocked is in-flight (pinned), no victim is
// evictable, so the new breaker is inserted anyway and Size temporarily exceeds
// Capacity by the number of concurrently in-flight new keys. It self-corrects
// as those calls drain. Route-level MaxInFlight bounds this overshoot in
// practice; size WithMaxBreakers for your trusted key cardinality plus expected
// concurrency headroom.
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
