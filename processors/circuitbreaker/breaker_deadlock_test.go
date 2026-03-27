package circuitbreaker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBreaker_OnStateChangeDoesNotDeadlock(t *testing.T) {
	cfg := Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		ResetTimeout:     50 * time.Millisecond,
	}

	done := make(chan struct{})
	b := newBreaker("deadlock-test", cfg, nil)
	b.onStateChange = func(key string, from, to State) {
		// Calling metrics() acquires b.mu — if transitionTo holds b.mu
		// while invoking this callback, this will deadlock.
		m := b.metrics()
		_ = m.Key
	}

	go func() {
		defer close(done)
		for i := 0; i < cfg.FailureThreshold; i++ {
			_ = b.beforeRequest()
			b.afterRequest(errors.New("fail"))
		}
	}()

	select {
	case <-done:
		// No deadlock
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock detected: onStateChange callback calling metrics() while transitionTo holds b.mu")
	}
}

func TestBreaker_OnStateChangeCallbackSafeConcurrent(t *testing.T) {
	cfg := Config{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		ResetTimeout:     50 * time.Millisecond,
	}

	var mu sync.Mutex
	var transitions []string

	b := newBreaker("safe-cb", cfg, nil)
	b.onStateChange = func(key string, from, to State) {
		m := b.metrics()
		mu.Lock()
		transitions = append(transitions, m.State)
		mu.Unlock()
	}

	for i := 0; i < cfg.FailureThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(errors.New("fail"))
	}

	time.Sleep(cfg.ResetTimeout + 10*time.Millisecond)

	for i := 0; i < cfg.SuccessThreshold; i++ {
		_ = b.beforeRequest()
		b.afterRequest(nil)
	}

	mu.Lock()
	if len(transitions) < 2 {
		t.Fatalf("expected at least 2 transitions, got %d", len(transitions))
	}
	mu.Unlock()
}
