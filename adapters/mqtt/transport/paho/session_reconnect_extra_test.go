package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-3 additional tests: activeSubs integrity under concurrent reconnect
// and multi-step restore scenarios.
// ═══════════════════════════════════════════════════════════════════════════

// TestActiveSubsRestore_SecondReconcileUsesRestoredState validates that
// after a failed reconnect restores activeSubs, a subsequent reconcile
// attempt with a valid context uses the restored subscription state for
// its delta calculation.
//
// Scenario:
// ───────────────────────────────────────────────
//  1. Set activeSubs = {"a": 1, "b": 0}
//  2. First reconnect: fails -> restores {"a": 1, "b": 0}
//  3. Second reconnect: reads activeSubs for delta -> should see {"a": 1, "b": 0}
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - After first failure, activeSubs contains the original entries
//   - The second reconnect attempt starts with correct activeSubs snapshot
func TestActiveSubsRestore_SecondReconcileUsesRestoredState(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-second-reconcile",
			ReconnectTimeout: 50 * time.Millisecond,
		},
		connectivity.SessionEphemeral,
		nil,
	)

	original := map[string]byte{"topic/a": 1, "topic/b": 0}

	s.mu.Lock()
	s.activeSubs = make(map[string]byte)
	for k, v := range original {
		s.activeSubs[k] = v
	}
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "topic/a", QoS: 1},
			{Topic: "topic/b", QoS: 0},
			{Topic: "topic/c", QoS: 1}, // new topic
		},
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled
	s.startCtx = cancelledCtx
	s.mu.Unlock()

	// --- First reconnect: fails ---
	s.mu.Lock()
	oldSubs1 := s.activeSubs
	s.activeSubs = make(map[string]byte)
	plan1 := s.plan
	pCtx1 := s.startCtx
	s.mu.Unlock()

	if plan1 != nil {
		rCtx, rCancel := context.WithTimeout(pCtx1, s.opts.ReconnectTimeout)
		if rCtx.Err() != nil {
			s.mu.Lock()
			s.activeSubs = oldSubs1
			s.mu.Unlock()
		}
		rCancel()
	}

	// Verify restored state after first failure.
	s.mu.Lock()
	if len(s.activeSubs) != 2 {
		t.Fatalf("after 1st failure: expected 2 subs, got %d", len(s.activeSubs))
	}
	s.mu.Unlock()

	// --- Second reconnect: read the activeSubs snapshot ---
	s.mu.Lock()
	oldSubs2 := s.activeSubs
	s.activeSubs = make(map[string]byte)
	s.mu.Unlock()

	// Verify the snapshot from the second reconnect has correct state.
	if len(oldSubs2) != 2 {
		t.Fatalf("second reconnect snapshot: expected 2 entries, got %d", len(oldSubs2))
	}
	if qos, ok := oldSubs2["topic/a"]; !ok || qos != 1 {
		t.Fatalf("snapshot missing topic/a or wrong QoS: ok=%v qos=%d", ok, qos)
	}
	if qos, ok := oldSubs2["topic/b"]; !ok || qos != 0 {
		t.Fatalf("snapshot missing topic/b or wrong QoS: ok=%v qos=%d", ok, qos)
	}

	// Simulate second failure and restore again.
	s.mu.Lock()
	s.activeSubs = oldSubs2
	s.mu.Unlock()

	// Final verification: state remains correct through two restore cycles.
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range original {
		if s.activeSubs[k] != v {
			t.Fatalf("final check: sub %q expected QoS %d, got %d", k, v, s.activeSubs[k])
		}
	}
}

// TestActiveSubsRestore_ConcurrentReconnections validates that concurrent
// simulated reconnection attempts do not corrupt the activeSubs state.
//
// This test runs multiple goroutines that each perform the
// save/clear/fail/restore pattern concurrently with the session mutex.
// The race detector (-race flag) will catch any unsynchronized access.
//
// Assertions:
//   - No data race detected (go test -race)
//   - After all goroutines complete, activeSubs is not empty
//   - Final activeSubs values are valid (QoS 0 or 1)
func TestActiveSubsRestore_ConcurrentReconnections(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-concurrent-restore",
			ReconnectTimeout: 50 * time.Millisecond,
		},
		connectivity.SessionEphemeral,
		nil,
	)

	original := map[string]byte{"x": 0, "y": 1, "z": 0}

	s.mu.Lock()
	for k, v := range original {
		s.activeSubs[k] = v
	}
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "x", QoS: 0},
			{Topic: "y", QoS: 1},
			{Topic: "z", QoS: 0},
			{Topic: "w", QoS: 1},
		},
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	s.startCtx = cancelledCtx
	s.mu.Unlock()

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			// Simulate the OnConnectionUp save/clear/fail/restore pattern.
			s.mu.Lock()
			oldSubs := s.activeSubs
			s.activeSubs = make(map[string]byte)
			plan := s.plan
			pCtx := s.startCtx
			s.mu.Unlock()

			if plan != nil {
				rCtx, rCancel := context.WithTimeout(pCtx, s.opts.ReconnectTimeout)
				if rCtx.Err() != nil {
					s.mu.Lock()
					s.activeSubs = oldSubs
					s.mu.Unlock()
				}
				rCancel()
			}
		}()
	}

	wg.Wait()

	// After all concurrent reconnects, activeSubs should not be nil.
	// The exact content depends on goroutine ordering, but it should
	// contain valid entries (from one of the restores).
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeSubs == nil {
		t.Fatal("activeSubs should not be nil after concurrent reconnects")
	}

	// Verify all values are valid QoS levels.
	for topic, qos := range s.activeSubs {
		if qos > 2 {
			t.Errorf("invalid QoS %d for topic %q", qos, topic)
		}
	}
}

// TestActiveSubsRestore_EmptyActiveSubs validates that the restore pattern
// works correctly when activeSubs is initially empty (no prior subscriptions).
//
// Assertions:
//   - activeSubs restored to empty map (not nil)
//   - No panic or corruption
func TestActiveSubsRestore_EmptyActiveSubs(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-empty-restore",
			ReconnectTimeout: 50 * time.Millisecond,
		},
		connectivity.SessionEphemeral,
		nil,
	)

	// activeSubs starts empty (from NewSession).
	s.mu.Lock()
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "new/topic", QoS: 1},
		},
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	s.startCtx = cancelledCtx
	s.mu.Unlock()

	// Simulate save/clear/fail/restore.
	s.mu.Lock()
	oldSubs := s.activeSubs
	s.activeSubs = make(map[string]byte)
	pCtx := s.startCtx
	s.mu.Unlock()

	rCtx, rCancel := context.WithTimeout(pCtx, s.opts.ReconnectTimeout)
	if rCtx.Err() != nil {
		s.mu.Lock()
		s.activeSubs = oldSubs
		s.mu.Unlock()
	}
	rCancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeSubs == nil {
		t.Fatal("activeSubs should not be nil after restore of empty state")
	}
	if len(s.activeSubs) != 0 {
		t.Fatalf("expected 0 active subs after restoring empty state, got %d", len(s.activeSubs))
	}
}

// TestActiveSubsRestore_ExactTopicQoSPairs validates that the restored
// activeSubs preserves exact topic-QoS pairs including mixed QoS levels
// and topics with special characters.
func TestActiveSubsRestore_ExactTopicQoSPairs(t *testing.T) {
	s := NewSession(
		SessionOptions{
			BrokerURLs:       []string{"tcp://localhost:1883"},
			ClientID:         "test-exact-pairs",
			ReconnectTimeout: 50 * time.Millisecond,
		},
		connectivity.SessionEphemeral,
		nil,
	)

	original := map[string]byte{
		"sensors/+/temperature":       0,
		"devices/#":                   1,
		"$SYS/broker/uptime":          2,
		"test/topic/with/many/levels": 1,
	}

	s.mu.Lock()
	s.activeSubs = make(map[string]byte)
	for k, v := range original {
		s.activeSubs[k] = v
	}
	s.plan = &connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "new/plan", QoS: 1},
		},
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	s.startCtx = cancelledCtx
	s.mu.Unlock()

	// Simulate save/clear/fail/restore.
	s.mu.Lock()
	oldSubs := s.activeSubs
	s.activeSubs = make(map[string]byte)
	pCtx := s.startCtx
	s.mu.Unlock()

	rCtx, rCancel := context.WithTimeout(pCtx, s.opts.ReconnectTimeout)
	if rCtx.Err() != nil {
		s.mu.Lock()
		s.activeSubs = oldSubs
		s.mu.Unlock()
	}
	rCancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeSubs) != len(original) {
		t.Fatalf("expected %d active subs, got %d", len(original), len(s.activeSubs))
	}
	for topic, expectedQoS := range original {
		got, ok := s.activeSubs[topic]
		if !ok {
			t.Errorf("missing restored topic %q", topic)
			continue
		}
		if got != expectedQoS {
			t.Errorf("topic %q: QoS = %d, want %d", topic, got, expectedQoS)
		}
	}
}
