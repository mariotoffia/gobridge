package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

func TestProcessor_EvictsOldestWhenAtCapacity(t *testing.T) {
	const capacity = 5
	cfg := cb.Config{
		FailureThreshold: 100,
		SuccessThreshold: 1,
		ResetTimeout:     time.Hour,
	}

	p := New("evict-test", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	next := func(_ context.Context, _ *messaging.Envelope) error { return nil }

	for i := 0; i < capacity; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: fmt.Sprintf("key-%d", i)})
		_ = p.Process(context.Background(), env, next)
	}

	if got := len(p.Metrics()); got != capacity {
		t.Fatalf("expected %d breakers, got %d", capacity, got)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "new-key-beyond-cap"})
	_ = p.Process(context.Background(), env, next)

	m := p.Metrics()
	if len(m) != capacity {
		t.Fatalf("expected %d breakers after eviction, got %d", capacity, len(m))
	}
	if _, ok := m["new-key-beyond-cap"]; !ok {
		t.Fatal("new key should exist after eviction")
	}

	if s := p.Stats(); s.Capacity != capacity || s.Size != capacity || s.Evictions != 1 || s.OpenEvictions != 0 {
		t.Fatalf("unexpected stats after evicting a closed breaker: %+v", s)
	}
}

// TestNew_MaxBreakersAndClockGuards locks the construction-time guards: a
// non-positive WithMaxBreakers keeps the default, a positive value is honoured,
// and a nil clock (via WithClock(nil)) is clamped so Process never dereferences
// a nil clock when it creates the first breaker.
func TestNew_MaxBreakersAndClockGuards(t *testing.T) {
	next := func(_ context.Context, _ *messaging.Envelope) error { return nil }
	cases := []struct {
		name string
		opts []Option
		want int
	}{
		{"default", nil, defaultMaxBreakers},
		{"zero_keeps_default", []Option{WithMaxBreakers(0)}, defaultMaxBreakers},
		{"negative_keeps_default", []Option{WithMaxBreakers(-5)}, defaultMaxBreakers},
		{"positive_honoured", []Option{WithMaxBreakers(7)}, 7},
		{"nil_clock_keeps_default", []Option{WithClock(nil)}, defaultMaxBreakers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("guard", cb.Config{}, tc.opts...)
			if got := p.Stats().Capacity; got != tc.want {
				t.Fatalf("Capacity = %d, want %d", got, tc.want)
			}
			// The nil-clock clamp is load-bearing: without it, creating the
			// first breaker with a nil clock would panic here.
			env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "guard-key"})
			if err := p.Process(context.Background(), env, next); err != nil {
				t.Fatalf("Process on freshly constructed processor: %v", err)
			}
		})
	}
}

func TestProcessor_EvictsClosedBreakerPreferentially(t *testing.T) {
	const capacity = 6
	cfg := cb.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     time.Hour,
	}

	p := New("evict-pref", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	next := func(_ context.Context, _ *messaging.Envelope) error { return nil }
	failNext := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("fail") }

	_ = p.Process(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "closed-key"}), next)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = p.Process(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "open-key"}), failNext)
	}

	m := p.Metrics()
	if m["open-key"].State != "open" {
		t.Fatalf("expected open-key to be open, got %s", m["open-key"].State)
	}
	if m["closed-key"].State != "closed" {
		t.Fatalf("expected closed-key to be closed, got %s", m["closed-key"].State)
	}

	p.mu.Lock()
	i := 0
	for len(p.breakers) < capacity {
		key := fmt.Sprintf("filler-%d", i)
		i++
		p.breakers[key] = &breakerEntry{breaker: cb.NewBreaker(key, cfg, nil)}
	}
	p.mu.Unlock()

	_ = p.Process(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "trigger-evict"}), next)

	m = p.Metrics()
	if _, ok := m["open-key"]; !ok {
		t.Fatal("open-key should survive eviction (not closed)")
	}
	if _, ok := m["trigger-evict"]; !ok {
		t.Fatal("trigger-evict should exist after eviction")
	}

	// A closed breaker was available, so the eviction must not have dropped an
	// open breaker.
	if s := p.Stats(); s.OpenEvictions != 0 {
		t.Fatalf("expected no open evictions when a closed breaker was available, got %+v", s)
	}
}

// TestProcessor_Stats_TracksOpenEvictions fills a small cache entirely with
// open breakers so the next distinct key forces the last-resort eviction of an
// open breaker. Stats().OpenEvictions is the churn red-flag operators watch to
// detect a cache sized below the (trusted) key cardinality.
func TestProcessor_Stats_TracksOpenEvictions(t *testing.T) {
	const capacity = 3
	cfg := cb.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     time.Hour,
	}

	p := New("open-evict", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	failNext := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("fail") }

	for i := 0; i < capacity; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: fmt.Sprintf("open-%d", i)})
		_ = p.Process(context.Background(), env, failNext)
	}
	for k, snap := range p.Metrics() {
		if snap.State != "open" {
			t.Fatalf("breaker %q expected open, got %s", k, snap.State)
		}
	}

	// The cache is full of open breakers: the new key must evict one of them.
	_ = p.Process(context.Background(), messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "overflow"}), failNext)

	s := p.Stats()
	if s.Capacity != capacity || s.Size != capacity {
		t.Fatalf("expected capacity and size %d, got %+v", capacity, s)
	}
	if s.Evictions != 1 || s.OpenEvictions != 1 {
		t.Fatalf("expected 1 eviction and 1 open eviction, got %+v", s)
	}
}

var _ ports.ProcessorFunc = func(_ context.Context, _ *messaging.Envelope) error { return nil }
