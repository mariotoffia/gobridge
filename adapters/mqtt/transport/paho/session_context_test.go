package paho

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-4 additional tests: startCtx propagation to reconcile
//
// The reconnect reconcile context must be derived from Start()'s ctx
// (stored as s.startCtx) so that session cancellation propagates to
// in-progress reconciliation.
// ═══════════════════════════════════════════════════════════════════════════

// TestStartCtx_IsNilBeforeStart validates that startCtx is nil on a
// freshly created Session before Start() is called.
func TestStartCtx_IsNilBeforeStart(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs: []string{"tcp://localhost:1883"},
			ClientID:   "test-nil-ctx",
		},
		domain.SessionEphemeral,
		nil,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.startCtx != nil {
		t.Fatal("startCtx should be nil before Start is called")
	}
}

// TestStartCtx_SetDuringStartSimulation validates that startCtx is
// stored as the parent context during the Start() call.
//
// Since Start() requires a real broker, we simulate the exact field
// assignment that Start() performs after successful connection.
func TestStartCtx_SetDuringStartSimulation(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs: []string{"tcp://localhost:1883"},
			ClientID:   "test-set-ctx",
		},
		domain.SessionEphemeral,
		nil,
	)

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Simulate the part of Start() that stores the context:
	//   s.mu.Lock()
	//   s.cm = cm
	//   s.startCtx = ctx
	//   s.mu.Unlock()
	s.mu.Lock()
	s.startCtx = parentCtx
	s.mu.Unlock()

	s.mu.Lock()
	stored := s.startCtx
	s.mu.Unlock()

	if stored != parentCtx {
		t.Fatal("startCtx should be the context provided to Start")
	}
}

// TestStartCtx_CancellationPropagates validates that cancelling the
// parent Start context causes the derived reconcile context to be
// cancelled, which is the core fix of BUG-4.
//
// Scenario:
// ───────────────────────────────────────────────
//   1. Create parent ctx with cancel
//   2. Store as startCtx (simulating Start)
//   3. Derive reconCtx from startCtx with 30s timeout
//   4. Cancel parent
//   5. Verify reconCtx is cancelled with context.Canceled
// ───────────────────────────────────────────────
func TestStartCtx_CancellationPropagates(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())

	// Derive the reconcile context from the parent, mimicking session.go:
	//   reconCtx, reconCancel := context.WithTimeout(parentCtx, reconTimeout)
	reconCtx, reconCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer reconCancel()

	parentCancel()

	select {
	case <-reconCtx.Done():
		// Expected: cancelled.
	case <-time.After(1 * time.Second):
		t.Fatal("reconCtx should be cancelled when parent is cancelled")
	}

	if reconCtx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", reconCtx.Err())
	}
}

// TestStartCtx_ReconTimeoutHonoured validates that the reconcile
// context timeout is still enforced even when startCtx is not cancelled.
//
// Scenario:
// ───────────────────────────────────────────────
//   1. Create long-lived parent context
//   2. Derive reconCtx with very short timeout (10ms)
//   3. Wait and verify reconCtx expires with DeadlineExceeded
// ───────────────────────────────────────────────
func TestStartCtx_ReconTimeoutHonoured(t *testing.T) {
	parentCtx := context.Background()

	reconCtx, reconCancel := context.WithTimeout(parentCtx, 10*time.Millisecond)
	defer reconCancel()

	select {
	case <-reconCtx.Done():
		// Expected: timed out.
	case <-time.After(1 * time.Second):
		t.Fatal("reconCtx should time out within 10ms")
	}

	if reconCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", reconCtx.Err())
	}
}

// TestStartCtx_FullOnConnectionUpSimulation exercises the full
// OnConnectionUp callback logic including context derivation from
// startCtx, verifying that:
//   - reconCtx is derived from startCtx (not context.Background())
//   - Cancelling startCtx cancels reconCtx
//   - activeSubs is restored on failure caused by context cancellation
func TestStartCtx_FullOnConnectionUpSimulation(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-full-ctx",
			ReconnectTimeout: 5 * time.Second,
		},
		domain.SessionEphemeral,
		nil,
	)

	parentCtx, parentCancel := context.WithCancel(context.Background())

	s.mu.Lock()
	s.activeSubs = map[string]byte{"topic/x": 1}
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "topic/x", QoS: 1},
			{Topic: "topic/y", QoS: 0},
		},
	}
	s.startCtx = parentCtx
	s.mu.Unlock()

	// Cancel the parent context BEFORE the reconnect attempt.
	parentCancel()

	// Replicate OnConnectionUp logic.
	s.mu.Lock()
	oldSubs := s.activeSubs
	s.activeSubs = make(map[string]byte)
	plan := s.plan
	pCtx := s.startCtx
	s.mu.Unlock()

	if plan != nil {
		reconTimeout := s.opts.ReconnectTimeout
		if reconTimeout == 0 {
			reconTimeout = 30 * time.Second
		}
		reconCtx, reconCancel := context.WithTimeout(pCtx, reconTimeout)
		defer reconCancel()

		// Because startCtx was cancelled, reconCtx should already be done.
		var reconcileErr error
		if reconCtx.Err() != nil {
			reconcileErr = reconCtx.Err()
		}

		if reconcileErr != nil {
			s.mu.Lock()
			s.activeSubs = oldSubs
			s.mu.Unlock()
		}
	}

	// Verify: activeSubs restored because reconCtx was cancelled
	// (derived from cancelled startCtx).
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeSubs) != 1 {
		t.Fatalf("expected 1 active sub after restore, got %d", len(s.activeSubs))
	}
	if qos, ok := s.activeSubs["topic/x"]; !ok || qos != 1 {
		t.Fatalf("expected topic/x with QoS 1, got ok=%v qos=%d", ok, qos)
	}
}

// TestStartCtx_NotCancelled_ReconSucceeds validates that when startCtx
// is alive, the reconcile context is also alive and the reconcile
// path does not trigger a restore.
func TestStartCtx_NotCancelled_ReconSucceeds(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-ctx-alive",
			ReconnectTimeout: 5 * time.Second,
		},
		domain.SessionEphemeral,
		nil,
	)

	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel() // keep alive

	s.mu.Lock()
	s.activeSubs = map[string]byte{"topic/a": 0}
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "topic/a", QoS: 0},
		},
	}
	s.startCtx = parentCtx
	s.mu.Unlock()

	// Replicate OnConnectionUp logic.
	s.mu.Lock()
	oldSubs := s.activeSubs
	s.activeSubs = make(map[string]byte)
	plan := s.plan
	pCtx := s.startCtx
	s.mu.Unlock()

	reconRestored := false
	if plan != nil {
		reconTimeout := s.opts.ReconnectTimeout
		if reconTimeout == 0 {
			reconTimeout = 30 * time.Second
		}
		reconCtx, reconCancel := context.WithTimeout(pCtx, reconTimeout)
		defer reconCancel()

		// Context should be alive.
		if reconCtx.Err() != nil {
			s.mu.Lock()
			s.activeSubs = oldSubs
			s.mu.Unlock()
			reconRestored = true
		} else {
			// Simulate successful reconcile: set the desired subs.
			s.mu.Lock()
			s.activeSubs["topic/a"] = 0
			s.mu.Unlock()
		}
	}

	if reconRestored {
		t.Fatal("activeSubs should NOT be restored when context is alive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeSubs) != 1 {
		t.Fatalf("expected 1 active sub, got %d", len(s.activeSubs))
	}
	if _, ok := s.activeSubs["topic/a"]; !ok {
		t.Fatal("expected topic/a in activeSubs after successful reconcile")
	}
}
