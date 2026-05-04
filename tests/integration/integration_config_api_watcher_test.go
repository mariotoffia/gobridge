package integration_test

import (
	"testing"
	"time"
)

// ===============================================================
// Group 5: Config API Full Pipeline (Watcher + Supervisor)
//
// Validates the end-to-end pipeline: HTTP commit writes to disk,
// file watcher detects the change (poll mode, 100ms), config
// manager re-merges, supervisor applies the new config.
//
// These tests start the full pipeline:
//   HTTP Server → configTxnManager → WriteFile → Watcher → Manager → Supervisor
// ===============================================================

// TestConfigAPI_Pipeline_CommitAddsRoute_SupervisorSwaps validates that
// committing a new route causes the supervisor to swap the runtime.
//
// Pipeline:
// ───────────────────────────────────────────────────────────
//
//	HTTP PATCH (add route r2) → commit → disk write
//	                                        ↓
//	                                file watcher (poll 100ms)
//	                                        ↓
//	                                config manager re-merge
//	                                        ↓
//	                                supervisor swap → runtime has r2
//
// ───────────────────────────────────────────────────────────
func TestConfigAPI_Pipeline_CommitAddsRoute_SupervisorSwaps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ps := newConfigAPITestServerWithPipeline(t, baseConfigForAPI())

	// Verify initial state: only r1 exists.
	pollForSupervisorRoute(t, ps.Supervisor, "r1", 3*time.Second)

	txnID := createTransaction(t, ps.URL, testAdminAPIKey)
	applyOverlay(t, ps.URL, testAdminAPIKey, txnID, map[string]any{
		"receivers": []map[string]any{{"id": "rx-2", "transport": "fake"}},
		"senders":   []map[string]any{{"id": "tx-2", "transport": "fake"}},
		"bindings":  []map[string]any{{"id": "bind-2", "sender_id": "tx-2", "address": "addr/2"}},
		"routes": []map[string]any{
			{"id": "r2", "receiver_id": "rx-2", "delivery_mode": "direct_hold", "bindings": []string{"bind-2"}},
		},
	})
	commitTransaction(t, ps.URL, testAdminAPIKey, txnID)

	// Poll until supervisor picks up the new route.
	pollForSupervisorRoute(t, ps.Supervisor, "r2", 5*time.Second)
}

// TestConfigAPI_Pipeline_CommitChangesLogLevel validates that a simple
// config change triggers a supervisor swap event.
func TestConfigAPI_Pipeline_CommitChangesLogLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ps := newConfigAPITestServerWithPipeline(t, baseConfigForAPI())

	txnID := createTransaction(t, ps.URL, testAdminAPIKey)
	applyOverlay(t, ps.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "error"},
	})
	commitTransaction(t, ps.URL, testAdminAPIKey, txnID)

	// Wait for a swap event.
	select {
	case ev := <-ps.SwapEvents:
		if ev.Error != nil {
			t.Fatalf("swap event error: %v", ev.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for swap event")
	}
}

// TestConfigAPI_Pipeline_RollbackDoesNotTriggerSwap validates that
// rolling back a transaction does not trigger a supervisor swap.
func TestConfigAPI_Pipeline_RollbackDoesNotTriggerSwap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ps := newConfigAPITestServerWithPipeline(t, baseConfigForAPI())

	txnID := createTransaction(t, ps.URL, testAdminAPIKey)
	applyOverlay(t, ps.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "error"},
	})
	rollbackTransaction(t, ps.URL, testAdminAPIKey, txnID)

	// Wait a bit — no swap event should arrive.
	select {
	case ev := <-ps.SwapEvents:
		t.Fatalf("unexpected swap event after rollback: %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// expected: no swap
	}
}

// TestConfigAPI_Pipeline_SequentialCommits_EachApplied validates that
// two sequential commits each trigger a supervisor swap.
func TestConfigAPI_Pipeline_SequentialCommits_EachApplied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ps := newConfigAPITestServerWithPipeline(t, baseConfigForAPI())

	// First commit: add route r2.
	txn1 := createTransaction(t, ps.URL, testAdminAPIKey)
	applyOverlay(t, ps.URL, testAdminAPIKey, txn1, map[string]any{
		"receivers": []map[string]any{{"id": "rx-2", "transport": "fake"}},
		"senders":   []map[string]any{{"id": "tx-2", "transport": "fake"}},
		"bindings":  []map[string]any{{"id": "bind-2", "sender_id": "tx-2", "address": "addr/2"}},
		"routes": []map[string]any{
			{"id": "r2", "receiver_id": "rx-2", "delivery_mode": "direct_hold", "bindings": []string{"bind-2"}},
		},
	})
	commitTransaction(t, ps.URL, testAdminAPIKey, txn1)
	pollForSupervisorRoute(t, ps.Supervisor, "r2", 5*time.Second)

	// Second commit: add route r3.
	txn2 := createTransaction(t, ps.URL, testAdminAPIKey)
	applyOverlay(t, ps.URL, testAdminAPIKey, txn2, map[string]any{
		"receivers": []map[string]any{{"id": "rx-3", "transport": "fake"}},
		"senders":   []map[string]any{{"id": "tx-3", "transport": "fake"}},
		"bindings":  []map[string]any{{"id": "bind-3", "sender_id": "tx-3", "address": "addr/3"}},
		"routes": []map[string]any{
			{"id": "r3", "receiver_id": "rx-3", "delivery_mode": "direct_hold", "bindings": []string{"bind-3"}},
		},
	})
	commitTransaction(t, ps.URL, testAdminAPIKey, txn2)
	pollForSupervisorRoute(t, ps.Supervisor, "r3", 5*time.Second)
}
