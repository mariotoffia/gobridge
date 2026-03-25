package circuitbreaker

import (
	"context"
	"sync"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Processor = (*Processor)(nil)

// Processor is a circuit breaker that tracks failures per key and short-circuits
// requests when a breaker trips open.
type Processor struct {
	name          string
	config        Config
	keyExtractor  KeyExtractor
	onStateChange func(key string, from, to State)
	mu            sync.Mutex
	breakers      map[string]*breaker
}

func New(name string, cfg Config, opts ...Option) *Processor {
	p := &Processor{
		name:         name,
		config:       cfg.withDefaults(),
		keyExtractor: GlobalKey,
		breakers:     make(map[string]*breaker),
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

func (p *Processor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	key := p.keyExtractor(ctx, env)

	p.mu.Lock()
	b, ok := p.breakers[key]
	if !ok {
		b = newBreaker(key, p.config, p.onStateChange)
		p.breakers[key] = b
	}
	p.mu.Unlock()

	if err := b.beforeRequest(); err != nil {
		return err
	}

	err := next(ctx, env)
	b.afterRequest(err)
	return err
}

func (p *Processor) Metrics() map[string]BreakerMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()

	m := make(map[string]BreakerMetrics, len(p.breakers))
	for k, b := range p.breakers {
		m[k] = b.metrics()
	}
	return m
}
