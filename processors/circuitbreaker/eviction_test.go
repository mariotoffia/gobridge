package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func TestProcessor_EvictsOldestWhenAtCapacity(t *testing.T) {
	cfg := Config{
		FailureThreshold: 100,
		SuccessThreshold: 1,
		ResetTimeout:     time.Hour,
	}

	p := New("evict-test", cfg, WithKeyExtractor(SubjectKey))

	next := func(_ context.Context, _ *domain.Envelope) error { return nil }

	for i := 0; i < maxBreakers; i++ {
		env := &domain.Envelope{Subject: fmt.Sprintf("key-%d", i)}
		_ = p.Process(context.Background(), env, next)
	}

	m := p.Metrics()
	if len(m) != maxBreakers {
		t.Fatalf("expected %d breakers, got %d", maxBreakers, len(m))
	}

	env := &domain.Envelope{Subject: "new-key-beyond-cap"}
	_ = p.Process(context.Background(), env, next)

	m = p.Metrics()
	if len(m) != maxBreakers {
		t.Fatalf("expected %d breakers after eviction, got %d", maxBreakers, len(m))
	}

	if _, ok := m["new-key-beyond-cap"]; !ok {
		t.Fatal("new key should exist after eviction")
	}
}

func TestProcessor_EvictsClosedBreakerPreferentially(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     time.Hour,
	}

	p := New("evict-pref", cfg, WithKeyExtractor(SubjectKey))

	next := func(_ context.Context, _ *domain.Envelope) error { return nil }
	failNext := func(_ context.Context, _ *domain.Envelope) error { return errors.New("fail") }

	p.Process(context.Background(), &domain.Envelope{Subject: "closed-key"}, next)

	for i := 0; i < cfg.FailureThreshold; i++ {
		p.Process(context.Background(), &domain.Envelope{Subject: "open-key"}, failNext)
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
	for len(p.breakers) < maxBreakers {
		key := fmt.Sprintf("filler-%d", i)
		i++
		p.breakers[key] = NewBreaker(key, cfg, nil)
	}
	p.mu.Unlock()

	p.Process(context.Background(), &domain.Envelope{Subject: "trigger-evict"}, next)

	m = p.Metrics()
	if _, ok := m["open-key"]; !ok {
		t.Fatal("open-key should survive eviction (not closed)")
	}
	if _, ok := m["trigger-evict"]; !ok {
		t.Fatal("trigger-evict should exist after eviction")
	}
}

var _ ports.ProcessorFunc = func(_ context.Context, _ *domain.Envelope) error { return nil }
