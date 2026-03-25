package circuitbreaker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

func nextOK(_ context.Context, _ *domain.Envelope) error { return nil }

func nextErr(sentinel error) func(context.Context, *domain.Envelope) error {
	return func(_ context.Context, _ *domain.Envelope) error { return sentinel }
}

func envelope(subject string, headers map[string]any) *domain.Envelope {
	return &domain.Envelope{Subject: subject, Headers: headers}
}

// Verifies closed-to-open-to-half-open-to-closed transitions under failures and reset timeout.
func TestStateTransitions_ClosedToOpenToHalfOpenToClosed(t *testing.T) {
	cfg := Config{FailureThreshold: 3, SuccessThreshold: 2, ResetTimeout: 50 * time.Millisecond}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)
	fail := nextErr(errors.New("boom"))

	for i := 0; i < 2; i++ {
		if err := p.Process(ctx, env, fail); err == nil {
			t.Fatalf("failure %d: expected error from next, got nil", i+1)
		}
	}

	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Fatalf("expected success while still closed (2 < threshold 3), got %v", err)
	}

	// Reset consecutive failures by the success above; need 3 fresh failures to trip.
	for i := 0; i < 3; i++ {
		if err := p.Process(ctx, env, fail); err == nil {
			t.Fatalf("failure %d: expected error from next, got nil", i+1)
		}
	}

	err := p.Process(ctx, env, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("circuit should be open: expected ErrUnavailable, got %v", err)
	}

	time.Sleep(60 * time.Millisecond)

	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Fatalf("half-open should allow request, got %v", err)
	}

	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Fatalf("second success should close circuit, got %v", err)
	}

	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Fatalf("closed circuit should pass through, got %v", err)
	}
}

// Verifies a failure in half-open reopens the circuit.
func TestHalfOpen_FailureReopens(t *testing.T) {
	cfg := Config{FailureThreshold: 2, SuccessThreshold: 2, ResetTimeout: 50 * time.Millisecond}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)
	fail := nextErr(errors.New("boom"))

	for i := 0; i < 2; i++ {
		p.Process(ctx, env, fail)
	}

	err := p.Process(ctx, env, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected open circuit, got %v", err)
	}

	time.Sleep(60 * time.Millisecond)

	if err := p.Process(ctx, env, fail); err == nil {
		t.Fatal("half-open failure: expected error from next, got nil")
	}

	err = p.Process(ctx, env, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("circuit should re-open after half-open failure, got %v", err)
	}
}

// Verifies circuit state is isolated per extracted key.
func TestPerKeyIsolation(t *testing.T) {
	cfg := Config{FailureThreshold: 2, SuccessThreshold: 1, ResetTimeout: 1 * time.Second}
	p := New("cb", cfg, WithKeyExtractor(SubjectKey))
	ctx := context.Background()
	fail := nextErr(errors.New("boom"))

	orders := envelope("orders", nil)
	for i := 0; i < 2; i++ {
		p.Process(ctx, orders, fail)
	}

	err := p.Process(ctx, orders, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("orders breaker should be open, got %v", err)
	}

	users := envelope("users", nil)
	if err := p.Process(ctx, users, nextOK); err != nil {
		t.Fatalf("users breaker should be closed, got %v", err)
	}

	m := p.Metrics()
	if _, ok := m["orders"]; !ok {
		t.Error("expected metrics key 'orders'")
	}
	if _, ok := m["users"]; !ok {
		t.Error("expected metrics key 'users'")
	}
	if len(m) != 2 {
		t.Errorf("expected 2 metric keys, got %d", len(m))
	}
}

// Verifies ErrUnavailable carries RetryAfter bounded by reset timeout.
func TestRetryAfterPropagation(t *testing.T) {
	cfg := Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: 200 * time.Millisecond}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)

	p.Process(ctx, env, nextErr(errors.New("boom")))

	err := p.Process(ctx, env, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}

	be, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatal("expected BridgeError")
	}
	if be.RetryAfter <= 0 {
		t.Errorf("RetryAfter should be > 0, got %v", be.RetryAfter)
	}
	if be.RetryAfter > 200*time.Millisecond {
		t.Errorf("RetryAfter should be <= 200ms, got %v", be.RetryAfter)
	}
}

// Verifies Process is safe under concurrent load and metrics count requests.
func TestConcurrentSafety(t *testing.T) {
	cfg := Config{FailureThreshold: 100, SuccessThreshold: 1, ResetTimeout: 1 * time.Second}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)

	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := p.Process(ctx, env, nextOK); err != nil {
					errCount.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	if errCount.Load() != 0 {
		t.Errorf("expected 0 errors, got %d", errCount.Load())
	}

	m := p.Metrics()
	got := m["global"].TotalRequests
	if got != goroutines*iterations {
		t.Errorf("TotalRequests = %d, want %d", got, goroutines*iterations)
	}
}

// Verifies GlobalKey, SubjectKey, and HeaderKey extractors.
func TestKeyExtractors(t *testing.T) {
	ctx := context.Background()

	t.Run("GlobalKey", func(t *testing.T) {
		if got := GlobalKey(ctx, envelope("anything", nil)); got != "global" {
			t.Errorf("GlobalKey = %q, want %q", got, "global")
		}
	})

	t.Run("SubjectKey", func(t *testing.T) {
		if got := SubjectKey(ctx, envelope("orders.new", nil)); got != "orders.new" {
			t.Errorf("SubjectKey = %q, want %q", got, "orders.new")
		}
	})

	t.Run("HeaderKey with value", func(t *testing.T) {
		env := envelope("test", map[string]any{domain.HeaderTenantID: "acme"})
		ke := HeaderKey(domain.HeaderTenantID)
		if got := ke(ctx, env); got != "acme" {
			t.Errorf("HeaderKey = %q, want %q", got, "acme")
		}
	})

	t.Run("HeaderKey missing", func(t *testing.T) {
		env := envelope("test", nil)
		ke := HeaderKey(domain.HeaderTenantID)
		if got := ke(ctx, env); got != "unknown" {
			t.Errorf("HeaderKey = %q, want %q", got, "unknown")
		}
	})
}

// Validates default failure threshold opens the circuit after five consecutive failures.
func TestConfigDefaults(t *testing.T) {
	p := New("", Config{})
	ctx := context.Background()
	env := envelope("test", nil)
	fail := nextErr(errors.New("boom"))

	// Default FailureThreshold is 5; 4 failures should keep circuit closed.
	for i := 0; i < 4; i++ {
		p.Process(ctx, env, fail)
	}
	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Fatalf("expected circuit still closed after 4 failures (default threshold 5), got %v", err)
	}

	// 5 consecutive failures (reset by the success above) to trip.
	for i := 0; i < 5; i++ {
		p.Process(ctx, env, fail)
	}
	err := p.Process(ctx, env, nextOK)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected open circuit after 5 failures, got %v", err)
	}
}

// Verifies Name returns the configured value or the default circuitbreaker.
func TestProcessorName(t *testing.T) {
	t.Run("custom name", func(t *testing.T) {
		p := New("my-cb", DefaultConfig())
		if got := p.Name(); got != "my-cb" {
			t.Errorf("Name() = %q, want %q", got, "my-cb")
		}
	})

	t.Run("empty name defaults", func(t *testing.T) {
		p := New("", DefaultConfig())
		if got := p.Name(); got != "circuitbreaker" {
			t.Errorf("Name() = %q, want %q", got, "circuitbreaker")
		}
	})
}

// Verifies metrics reflect successes, failures, and open state.
func TestMetrics(t *testing.T) {
	cfg := Config{FailureThreshold: 3, SuccessThreshold: 1, ResetTimeout: 1 * time.Second}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)
	fail := nextErr(errors.New("boom"))

	for i := 0; i < 2; i++ {
		p.Process(ctx, env, nextOK)
	}
	for i := 0; i < 3; i++ {
		p.Process(ctx, env, fail)
	}

	m, ok := p.Metrics()["global"]
	if !ok {
		t.Fatal("expected metrics key 'global'")
	}

	if m.TotalRequests != 5 {
		t.Errorf("TotalRequests = %d, want 5", m.TotalRequests)
	}
	if m.TotalSuccesses != 2 {
		t.Errorf("TotalSuccesses = %d, want 2", m.TotalSuccesses)
	}
	if m.TotalFailures != 3 {
		t.Errorf("TotalFailures = %d, want 3", m.TotalFailures)
	}
	if m.State != "open" {
		t.Errorf("State = %q, want %q", m.State, "open")
	}
	if m.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", m.ConsecutiveFailures)
	}
	if m.ConsecutiveSuccesses != 0 {
		t.Errorf("ConsecutiveSuccesses = %d, want 0", m.ConsecutiveSuccesses)
	}
}

// Verifies OnStateChange receives closed-to-open, open-to-half-open, and half-open-to-closed transitions.
func TestOnStateChangeCallback(t *testing.T) {
	cfg := Config{FailureThreshold: 2, SuccessThreshold: 1, ResetTimeout: 50 * time.Millisecond}

	var mu sync.Mutex
	var transitions []struct{ from, to State }

	p := New("cb", cfg, WithOnStateChange(func(key string, from, to State) {
		mu.Lock()
		transitions = append(transitions, struct{ from, to State }{from, to})
		mu.Unlock()
	}))

	ctx := context.Background()
	env := envelope("test", nil)
	fail := nextErr(errors.New("boom"))

	for i := 0; i < 2; i++ {
		p.Process(ctx, env, fail)
	}

	mu.Lock()
	if len(transitions) != 1 || transitions[0].from != StateClosed || transitions[0].to != StateOpen {
		t.Errorf("expected [closed->open], got %v", transitions)
	}
	mu.Unlock()

	time.Sleep(60 * time.Millisecond)

	// Triggers half-open transition inside beforeRequest.
	p.Process(ctx, env, nextOK)

	mu.Lock()
	if len(transitions) < 2 {
		t.Fatalf("expected at least 2 transitions, got %d", len(transitions))
	}
	if transitions[1].from != StateOpen || transitions[1].to != StateHalfOpen {
		t.Errorf("expected [open->half-open], got %v", transitions[1])
	}
	// SuccessThreshold=1, so same request closes it.
	if len(transitions) < 3 {
		t.Fatalf("expected 3 transitions, got %d", len(transitions))
	}
	if transitions[2].from != StateHalfOpen || transitions[2].to != StateClosed {
		t.Errorf("expected [half-open->closed], got %v", transitions[2])
	}
	mu.Unlock()
}

// Verifies next-handler errors propagate and count as failures while the circuit stays closed.
func TestNextErrorPropagation(t *testing.T) {
	cfg := Config{FailureThreshold: 10, SuccessThreshold: 1, ResetTimeout: 1 * time.Second}
	p := New("cb", cfg)
	ctx := context.Background()
	env := envelope("test", nil)

	sentinel := errors.New("downstream failure")
	err := p.Process(ctx, env, nextErr(sentinel))
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}

	m := p.Metrics()["global"]
	if m.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1", m.TotalFailures)
	}

	if err := p.Process(ctx, env, nextOK); err != nil {
		t.Errorf("circuit should still be closed, got %v", err)
	}
}
