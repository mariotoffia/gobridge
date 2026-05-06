package circuitbreaker

import (
	"context"
	"fmt"
	"sync"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Processor = (*Processor)(nil)

// Processor is a circuit breaker that tracks failures per key and short-circuits
// requests when a breaker trips open.
type Processor struct {
	name          string
	config        cb.Config
	keyExtractor  KeyExtractor
	onStateChange func(key string, from, to cb.State)
	mu            sync.Mutex
	breakers      map[string]*cb.Breaker
}

func New(name string, cfg cb.Config, opts ...Option) *Processor {
	p := &Processor{
		name:         name,
		config:       cfg.WithDefaults(),
		keyExtractor: GlobalKey,
		breakers:     make(map[string]*cb.Breaker),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Processor) Name() string {
	if p.name == "" {
		return "circuitbreaker"
	}
	return p.name
}

const maxBreakers = 10000

func (p *Processor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	key := p.keyExtractor(ctx, env)

	p.mu.Lock()
	b, ok := p.breakers[key]
	if !ok {
		if len(p.breakers) >= maxBreakers {
			p.evictOldest()
		}
		b = cb.NewBreaker(key, p.config, p.onStateChange)
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
		return
	}
	// Fallback: evict a half-open breaker. Never evict open breakers as
	// they protect against known-failing dependencies.
	for k, b := range p.breakers {
		m := b.GetMetrics()
		if m.State == cb.StateHalfOpen.String() {
			delete(p.breakers, k)
			return
		}
	}
	// Last resort: evict any breaker (including open) to prevent unbounded growth.
	for k := range p.breakers {
		delete(p.breakers, k)
		return
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
