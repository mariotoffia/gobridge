package circuitbreaker

import (
	"context"
	"fmt"
	"sync"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
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
	name           string
	config         cb.Config
	keyExtractor   KeyExtractor
	onStateChange  func(key string, from, to cb.State)
	maxBreakers    int
	clk            clock.Clock
	mu             sync.Mutex
	breakers       map[string]*cb.Breaker
	totalEvictions int64
	openEvictions  int64
}

func New(name string, cfg cb.Config, opts ...Option) *Processor {
	p := &Processor{
		name:         name,
		config:       cfg.WithDefaults(),
		keyExtractor: GlobalKey,
		maxBreakers:  defaultMaxBreakers,
		clk:          clock.System,
		breakers:     make(map[string]*cb.Breaker),
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
	b, ok := p.breakers[key]
	if !ok {
		if len(p.breakers) >= p.maxBreakers {
			p.evictOldest()
		}
		b = cb.NewBreaker(key, p.config, p.onStateChange, cb.WithBreakerClock(p.clk))
		p.breakers[key] = b
	}
	p.mu.Unlock()

	if err := b.BeforeRequest(); err != nil {
		return err
	}

	var err error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("circuitbreaker: panic in processor chain: %v", r)
			b.AfterRequest(err)
			panic(r)
		}
		b.AfterRequest(err)
	}()

	err = next(ctx, env)
	return err
}

// evictOldest makes room in a full breaker cache. It prefers the
// least-recently-failed CLOSED breaker, then a half-open one, and only as a
// last resort an open breaker -- open breakers protect against known-failing
// dependencies, so evicting one is counted separately in Stats().OpenEvictions
// as a churn signal. Called with p.mu held.
func (p *Processor) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, b := range p.breakers {
		m := b.GetMetrics()
		if m.State == cb.StateClosed.String() {
			if first || m.LastFailureTime.Before(oldestTime) {
				oldestKey = k
				oldestTime = m.LastFailureTime
				first = false
			}
		}
	}
	if oldestKey != "" {
		delete(p.breakers, oldestKey)
		p.totalEvictions++
		return
	}
	// Fallback: evict a half-open breaker. Never evict open breakers here as
	// they protect against known-failing dependencies.
	for k, b := range p.breakers {
		m := b.GetMetrics()
		if m.State == cb.StateHalfOpen.String() {
			delete(p.breakers, k)
			p.totalEvictions++
			return
		}
	}
	// Last resort: no closed or half-open breaker remains, so every entry is
	// open. Evict one to bound growth, but record it as an open eviction --
	// dropping an open breaker sheds active protection and means the cache is
	// too small for the key cardinality. Confirm the victim is still open when
	// counting: breaker state is mutated outside p.mu, so a breaker can
	// transition between the scans above and this loop; count only a genuine
	// open drop so OpenEvictions stays exact.
	for k, b := range p.breakers {
		delete(p.breakers, k)
		p.totalEvictions++
		if b.GetMetrics().State == cb.StateOpen.String() {
			p.openEvictions++
		}
		return
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
	for k, b := range p.breakers {
		m[k] = b.GetMetrics()
	}
	return m
}
