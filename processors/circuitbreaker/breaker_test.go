package circuitbreaker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var errTest = errors.New("test failure")

func fastConfig() Config {
	return Config{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		ResetTimeout:     50 * time.Millisecond,
	}
}

func TestBreaker_StartsInClosedState(t *testing.T) {
	b := newBreaker("test", fastConfig(), nil)
	m := b.metrics()
	if m.State != "closed" {
		t.Fatalf("expected closed, got %s", m.State)
	}
}

func TestBreaker_OpensAfterFailureThreshold(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		if err := b.beforeRequest(); err != nil {
			t.Fatalf("unexpected error on request %d: %v", i, err)
		}
		b.afterRequest(errTest)
	}

	m := b.metrics()
	if m.State != "open" {
		t.Fatalf("expected open after %d failures, got %s", cfg.FailureThreshold, m.State)
	}
}

func TestBreaker_OpenRejectsRequests(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	err := b.beforeRequest()
	if err == nil {
		t.Fatal("expected error from open breaker, got nil")
	}
}

func TestBreaker_TransitionsToHalfOpenAfterTimeout(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond)

	err := b.beforeRequest()
	if err != nil {
		t.Fatalf("expected nil after reset timeout, got: %v", err)
	}

	m := b.metrics()
	if m.State != "half-open" {
		t.Fatalf("expected half-open, got %s", m.State)
	}
}

func TestBreaker_HalfOpenToClosedAfterSuccessThreshold(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond)

	for i := 0; i < cfg.SuccessThreshold; i++ {
		if err := b.beforeRequest(); err != nil {
			t.Fatalf("unexpected error in half-open: %v", err)
		}
		b.afterRequest(nil)
	}

	m := b.metrics()
	if m.State != "closed" {
		t.Fatalf("expected closed after %d successes, got %s", cfg.SuccessThreshold, m.State)
	}
}

func TestBreaker_HalfOpenToOpenOnFailure(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond)

	_ = b.beforeRequest()
	b.afterRequest(errTest)

	m := b.metrics()
	if m.State != "open" {
		t.Fatalf("expected open after half-open failure, got %s", m.State)
	}
}

func TestBreaker_SuccessResetsConsecutiveFailures(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold-1; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	_ = b.beforeRequest()
	b.afterRequest(nil)

	m := b.metrics()
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures after success, got %d", m.ConsecutiveFailures)
	}
	if m.State != "closed" {
		t.Fatalf("expected still closed, got %s", m.State)
	}
}

func TestBreaker_OnStateChangeCallback(t *testing.T) {
	var transitions []struct{ from, to State }
	var mu sync.Mutex

	cfg := fastConfig()
	b := newBreaker("cb-test", cfg, func(key string, from, to State) {
		mu.Lock()
		transitions = append(transitions, struct{ from, to State }{from, to})
		mu.Unlock()
	})

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	mu.Lock()
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].from != StateClosed || transitions[0].to != StateOpen {
		t.Fatalf("expected closed->open, got %v->%v", transitions[0].from, transitions[0].to)
	}
	mu.Unlock()
}

func TestBreaker_MetricsCounters(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("metrics-test", cfg, nil)

	_ = b.beforeRequest()
	b.afterRequest(nil)
	_ = b.beforeRequest()
	b.afterRequest(errTest)

	m := b.metrics()
	if m.TotalRequests != 2 {
		t.Fatalf("expected 2 total requests, got %d", m.TotalRequests)
	}
	if m.TotalSuccesses != 1 {
		t.Fatalf("expected 1 success, got %d", m.TotalSuccesses)
	}
	if m.TotalFailures != 1 {
		t.Fatalf("expected 1 failure, got %d", m.TotalFailures)
	}
}

func TestBreaker_ConcurrentAccess(t *testing.T) {
	cfg := Config{
		FailureThreshold: 100,
		SuccessThreshold: 10,
		ResetTimeout:     50 * time.Millisecond,
	}
	b := newBreaker("concurrent", cfg, nil)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := b.beforeRequest(); err != nil {
				return
			}
			if idx%3 == 0 {
				b.afterRequest(errTest)
			} else {
				b.afterRequest(nil)
			}
		}(i)
	}
	wg.Wait()

	m := b.metrics()
	if m.TotalRequests == 0 {
		t.Fatal("expected some requests to be processed")
	}
}

func TestBreaker_BoundaryOneFailureBelowThreshold(t *testing.T) {
	cfg := fastConfig()
	b := newBreaker("boundary", cfg, nil)

	for i := 0; i < cfg.FailureThreshold-1; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errTest)
	}

	m := b.metrics()
	if m.State != "closed" {
		t.Fatalf("expected closed at threshold-1, got %s", m.State)
	}
	if m.ConsecutiveFailures != cfg.FailureThreshold-1 {
		t.Fatalf("expected %d consecutive failures, got %d", cfg.FailureThreshold-1, m.ConsecutiveFailures)
	}
}
