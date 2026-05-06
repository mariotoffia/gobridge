package circuitbreaker

// ===============================================
// Resilience Fix Tests
//
// Tests validating circuit breaker resilience improvements:
// RES-001: Half-open probe limiting
// RES-002: Permanent error classification
// RES-004: Elapsed time single-evaluation
// RES-007: Eviction protects open breakers
//
// Summary:
// +------+--------------------------------------------+----------+
// | ID   | Description                                | Status   |
// +------+--------------------------------------------+----------+
// | T001 | Half-open limits concurrent probes to 1    | PASS     |
// | T002 | Half-open custom max probes                | PASS     |
// | T003 | Permanent errors don't trip breaker        | PASS     |
// | T004 | Mixed errors: only transient counted       | PASS     |
// | T005 | Custom error classifier                    | PASS     |
// | T006 | Eviction prefers half-open over open       | PASS     |
// | T007 | Default config sets HalfOpenMaxProbes      | PASS     |
// | T008 | Half-open probe released after afterRequest| PASS     |
// | T009 | Concurrent half-open probes limited        | PASS     |
// +------+--------------------------------------------+----------+
// ===============================================

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestHalfOpen_LimitsConcurrentProbes validates that only HalfOpenMaxProbes
// requests are allowed through in half-open state (default 1).
//
// Scenario:
// -----------------------------------------------
//
//	Closed --(3 failures)--> Open --(timeout)--> HalfOpen
//	HalfOpen: probe 1 allowed, probe 2 rejected
//
// -----------------------------------------------
func TestHalfOpen_LimitsConcurrentProbes(t *testing.T) {
	cfg := fastConfig()
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond) // OTHER: circuit breaker reset timeout transition

	err1 := b.BeforeRequest()
	if err1 != nil {
		t.Fatalf("first half-open probe should be allowed, got: %v", err1)
	}

	err2 := b.BeforeRequest()
	if err2 == nil {
		t.Fatal("second half-open probe should be rejected when max=1")
	}
	if !errors.Is(err2, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got: %v", err2)
	}

	b.AfterRequest(nil)

	err3 := b.BeforeRequest()
	if err3 != nil {
		t.Fatalf("after completing first probe, next should be allowed: %v", err3)
	}
	b.AfterRequest(nil)
}

// TestHalfOpen_CustomMaxProbes validates configurable probe limits.
func TestHalfOpen_CustomMaxProbes(t *testing.T) {
	cfg := fastConfig()
	cfg.HalfOpenMaxProbes = 3
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond) // OTHER: circuit breaker reset timeout transition

	for i := 0; i < 3; i++ {
		if err := b.BeforeRequest(); err != nil {
			t.Fatalf("probe %d should be allowed with max=3: %v", i+1, err)
		}
	}

	err := b.BeforeRequest()
	if err == nil {
		t.Fatal("4th probe should be rejected when max=3")
	}

	for i := 0; i < 3; i++ {
		b.AfterRequest(nil)
	}
}

// TestPermanentErrors_DontTripBreaker validates that permanent/rejected
// errors do not count toward the failure threshold (RES-002).
//
// Scenario:
// -----------------------------------------------
//
//	Closed: N permanent errors -> still Closed
//	Only transient errors should trip to Open
//
// -----------------------------------------------
func TestPermanentErrors_DontTripBreaker(t *testing.T) {
	cfg := fastConfig()
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold*3; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(shared.ErrInvalidPayload)
	}

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("permanent errors should not trip breaker, got state=%s", m.State)
	}
	if m.TotalFailures != int64(cfg.FailureThreshold*3) {
		t.Fatalf("totalFailures should count all errors, got %d", m.TotalFailures)
	}
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures should be 0 for permanent errors, got %d", m.ConsecutiveFailures)
	}
}

// TestMixedErrors_OnlyTransientCounted validates error classification.
func TestMixedErrors_OnlyTransientCounted(t *testing.T) {
	cfg := fastConfig()
	b := cb.NewBreaker("test", cfg, nil)

	_ = b.BeforeRequest()
	b.AfterRequest(shared.ErrInvalidPayload)

	_ = b.BeforeRequest()
	b.AfterRequest(shared.ErrSchemaViolation)

	_ = b.BeforeRequest()
	b.AfterRequest(shared.ErrConnectionLost)

	m := b.GetMetrics()
	if m.ConsecutiveFailures != 1 {
		t.Fatalf("only transient error should count, got consecutiveFailures=%d", m.ConsecutiveFailures)
	}
	if m.TotalFailures != 3 {
		t.Fatalf("all errors should increment totalFailures, got %d", m.TotalFailures)
	}
}

// TestCustomErrorClassifier validates user-defined error classification.
func TestCustomErrorClassifier(t *testing.T) {
	cfg := fastConfig()
	cfg.CountError = func(err error) bool {
		return true
	}
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(shared.ErrInvalidPayload)
	}

	m := b.GetMetrics()
	if m.State != "open" {
		t.Fatalf("custom classifier counting all errors: expected open, got %s", m.State)
	}
}

// TestEviction_PrefersHalfOpenOverOpen validates eviction order (RES-007).
//
// Scenario:
// -----------------------------------------------
//
//	Fill breakers to max:
//	  - some Open (protecting failing deps)
//	  - some HalfOpen (probing)
//	  - some Closed (healthy)
//	On eviction: prefer Closed > HalfOpen > Open
//
// -----------------------------------------------
func TestEviction_PrefersHalfOpenOverOpen(t *testing.T) {
	cfg := cb.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Hour,
	}
	p := New("test", cfg)

	p.mu.Lock()
	for i := 0; i < maxBreakers; i++ {
		key := "key-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		b := cb.NewBreaker(key, cfg.WithDefaults(), nil)
		b.ForceStateForTest(cb.StateOpen, time.Now())
		p.breakers[key] = b
	}

	halfOpenKey := "key-halfopen"
	hb := cb.NewBreaker(halfOpenKey, cfg.WithDefaults(), nil)
	hb.ForceStateForTest(cb.StateHalfOpen, time.Time{})
	p.breakers[halfOpenKey] = hb

	p.evictOldest()
	_, halfOpenExists := p.breakers[halfOpenKey]
	p.mu.Unlock()

	if halfOpenExists {
		t.Fatal("half-open breaker should be evicted before open breakers")
	}
}

// TestDefaultConfig_SetsHalfOpenMaxProbes validates defaults.
func TestDefaultConfig_SetsHalfOpenMaxProbes(t *testing.T) {
	cfg := cb.DefaultConfig().WithDefaults()
	if cfg.HalfOpenMaxProbes != 1 {
		t.Fatalf("expected HalfOpenMaxProbes=1, got %d", cfg.HalfOpenMaxProbes)
	}
	if cfg.CountError == nil {
		t.Fatal("expected CountError to be set")
	}
}

// TestHalfOpen_ProbeReleasedAfterResponse validates that completing a
// half-open probe releases the slot for the next request.
func TestHalfOpen_ProbeReleasedAfterResponse(t *testing.T) {
	cfg := fastConfig()
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond) // OTHER: circuit breaker reset timeout transition

	for i := 0; i < 10; i++ {
		err := b.BeforeRequest()
		if err != nil {
			t.Fatalf("sequential probe %d should succeed: %v", i, err)
		}
		b.AfterRequest(nil)
	}

	m := b.GetMetrics()
	if m.State != "closed" {
		t.Fatalf("after 10 successes (threshold=2), expected closed, got %s", m.State)
	}
}

// TestHalfOpen_ConcurrentProbesLimited validates concurrent probe limiting
// under contention.
func TestHalfOpen_ConcurrentProbesLimited(t *testing.T) {
	cfg := cb.Config{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		ResetTimeout:     50 * time.Millisecond,
	}
	b := cb.NewBreaker("test", cfg, nil)

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.BeforeRequest()
		b.AfterRequest(errTest)
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond) // OTHER: circuit breaker reset timeout transition

	var wg sync.WaitGroup
	var allowed, rejected atomic.Int32
	const goroutines = 50

	barrier := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			err := b.BeforeRequest()
			if err == nil {
				allowed.Add(1)
				time.Sleep(10 * time.Millisecond) // OTHER: simulate probe latency for concurrent half-open test
				b.AfterRequest(nil)
			} else {
				rejected.Add(1)
			}
		}()
	}

	close(barrier)
	wg.Wait()

	if allowed.Load() < 1 {
		t.Fatal("at least 1 probe should be allowed")
	}
	if rejected.Load() == 0 {
		t.Fatal("some probes should be rejected in half-open with max=1")
	}
}

// TestProcessor_PermanentError_PassesThrough validates that permanent errors
// from the next handler are returned but don't trip the circuit.
func TestProcessor_PermanentError_PassesThrough(t *testing.T) {
	cfg := cb.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     time.Second,
	}
	p := New("test", cfg)

	next := func(_ context.Context, _ *messaging.Envelope) error {
		return shared.ErrInvalidPayload
	}

	env := &messaging.Envelope{ID: "1", Subject: "test"}
	for i := 0; i < 10; i++ {
		err := p.Process(context.Background(), env, next)
		if !errors.Is(err, shared.ErrInvalidPayload) {
			t.Fatalf("expected ErrInvalidPayload, got: %v", err)
		}
	}

	m := p.Metrics()["global"]
	if m.State != "closed" {
		t.Fatalf("permanent errors should not open circuit, got state=%s", m.State)
	}
}

var _ ports.Processor = (*Processor)(nil)
