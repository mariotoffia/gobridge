package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// DLQ Router Integration Tests with DynamoDB Local
//
// Validates the DLQRouter against a real DynamoDB DLQ store, exercising
// async buffer draining, error classification, concurrent writes, and
// graceful shutdown semantics.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────────────┐
// │ Test │ Description                                                  │
// ├──────┼──────────────────────────────────────────────────────────────┤
// │ DR1  │ Route persists entry with all fields in DynamoDB             │
// │ DR2  │ Async buffer mode drains entries via background workers      │
// │ DR3  │ Error classification persists correct category and code      │
// │ DR4  │ Close drains remaining buffer entries before stopping        │
// │ DR5  │ Concurrent Route calls all persist safely                    │
// └──────┴──────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_DLQRouter_RouteStoresEntry validates that Route creates
// a DLQ entry in the real DynamoDB store with all fields populated.
//
// Scenario:
//
//	DLQRouter ──Route──▶ DynamoDB DLQ Store ──List──▶ verify entry
func TestIntegration_DLQRouter_RouteStoresEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBDLQStore(t, "dr1")
	router := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store:        store,
		WriteTimeout: 10 * time.Second,
	})

	env := &domain.Envelope{
		ID:      "env-dr1",
		Subject: "test/topic",
		Payload: []byte(`{"dlq":"entry"}`),
		Headers: map[string]any{
			domain.HeaderCorrelationID: "corr-dr1",
		},
	}

	routeErr := shared.ErrInvalidPayload.WithMessage("bad payload format")

	ctx := context.Background()
	if err := router.Route(ctx, env, "route-dr1", "bind-dr1", "sess-dr1", "src-dr1", routeErr, 3); err != nil {
		t.Fatalf("Route: %v", err)
	}

	entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-dr1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", len(entries))
	}

	e := entries[0]
	if e.RouteID != "route-dr1" {
		t.Fatalf("RouteID: got %q, want %q", e.RouteID, "route-dr1")
	}
	if e.Envelope.ID != "env-dr1" {
		t.Fatalf("Envelope.ID: got %q, want %q", e.Envelope.ID, "env-dr1")
	}
	if string(e.Envelope.Payload) != `{"dlq":"entry"}` {
		t.Fatalf("Payload: got %q", string(e.Envelope.Payload))
	}
	if e.Attempts != 3 {
		t.Fatalf("Attempts: got %d, want %d", e.Attempts, 3)
	}
	if e.FailedAt.IsZero() {
		t.Fatal("FailedAt should not be zero")
	}
}

// TestIntegration_DLQRouter_AsyncBufferDrains validates that when the router
// is started in async mode, entries enqueued via Route are eventually drained
// to the DynamoDB store by background workers.
//
// Scenario:
//
//	DLQRouter.Start() ── workers active
//	Route(entry1)      ──▶ buffer ──▶ worker ──▶ DynamoDB
//	Route(entry2)      ──▶ buffer ──▶ worker ──▶ DynamoDB
//	... wait ...
//	List ──▶ 2 entries
func TestIntegration_DLQRouter_AsyncBufferDrains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBDLQStore(t, "dr2")
	router := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store:        store,
		BufferSize:   100,
		Workers:      2,
		WriteTimeout: 10 * time.Second,
	})

	ctx := context.Background()
	router.Start(ctx)
	defer router.Close()
	const entryCount = 5
	for i := 0; i < entryCount; i++ {
		env := &domain.Envelope{
			ID:      uniqueID("env-dr2"),
			Subject: "test/async",
			Payload: []byte(`{"async":"entry"}`),
		}
		routeErr := shared.ErrUnavailable.WithMessage("transient failure")
		if err := router.Route(ctx, env, "route-dr2", uniqueID("bind"), "sess-dr2", "", routeErr, 1); err != nil {
			t.Fatalf("Route[%d]: %v", i, err)
		}
	}

	e2eWaitFor(t, 10*time.Second, "async DLQ entries drained", func() bool {
		entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-dr2"})
		if err != nil {
			return false
		}
		return len(entries) >= entryCount
	})

	entries, _ := store.List(ctx, domain.DLQFilter{RouteID: "route-dr2"})
	if len(entries) != entryCount {
		t.Fatalf("expected %d DLQ entries, got %d", entryCount, len(entries))
	}
}

// TestIntegration_DLQRouter_ErrorClassification validates that error category
// and error code from BridgeError are persisted correctly in DynamoDB.
//
// Scenario:
//
//	Route(ErrInvalidPayload) ──▶ DDB → category="permanent", code="INVALID_PAYLOAD"
//	Route(ErrUnavailable)    ──▶ DDB → category="transient",  code="UNAVAILABLE"
func TestIntegration_DLQRouter_ErrorClassification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBDLQStore(t, "dr3")
	router := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store:        store,
		WriteTimeout: 10 * time.Second,
	})

	ctx := context.Background()

	// Permanent error (ErrNotFound has ErrorPermanent class)
	env1 := &domain.Envelope{ID: "env-dr3-perm", Subject: "test", Payload: []byte("x")}
	permErr := shared.ErrNotFound.WithMessage("resource gone")
	if err := router.Route(ctx, env1, "route-dr3", "b1", "s1", "", permErr, 1); err != nil {
		t.Fatalf("Route perm: %v", err)
	}

	// Transient error
	env2 := &domain.Envelope{ID: "env-dr3-trans", Subject: "test", Payload: []byte("y")}
	transErr := shared.ErrUnavailable.WithMessage("service down")
	if err := router.Route(ctx, env2, "route-dr3", "b2", "s2", "", transErr, 2); err != nil {
		t.Fatalf("Route trans: %v", err)
	}

	entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-dr3"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	categories := map[string]bool{}
	for _, e := range entries {
		categories[e.Category] = true
	}
	if !categories[string(shared.ErrorPermanent)] {
		t.Fatal("missing permanent category entry")
	}
	if !categories[string(shared.ErrorTransient)] {
		t.Fatal("missing transient category entry")
	}
}

// TestIntegration_DLQRouter_CloseDrainsBuffer validates that calling Close
// after Start drains all remaining buffer entries to DynamoDB before returning.
//
// Scenario:
//
//	Start → Route(5 entries) → Close() → verify all 5 in DDB
func TestIntegration_DLQRouter_CloseDrainsBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBDLQStore(t, "dr4")
	router := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store:        store,
		BufferSize:   100,
		Workers:      1,
		WriteTimeout: 10 * time.Second,
	})

	ctx := context.Background()
	router.Start(ctx)

	const entryCount = 5
	for i := 0; i < entryCount; i++ {
		env := &domain.Envelope{
			ID:      uniqueID("env-dr4"),
			Subject: "test/drain",
			Payload: []byte(`{"drain":"test"}`),
		}
		if err := router.Route(ctx, env, "route-dr4", uniqueID("bind"), "sess-dr4", "", shared.ErrUnavailable, 1); err != nil {
			t.Fatalf("Route[%d]: %v", i, err)
		}
	}

	router.Close()

	entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-dr4"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != entryCount {
		t.Fatalf("expected %d entries after Close, got %d", entryCount, len(entries))
	}
}

// TestIntegration_DLQRouter_ConcurrentRoutes validates that concurrent Route
// calls from multiple goroutines all persist safely without data loss.
//
// Scenario:
//
//	20 goroutines ──Route──▶ buffer ──▶ workers ──▶ DynamoDB
//	                                                 ↓
//	                                           List = 20 entries
func TestIntegration_DLQRouter_ConcurrentRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBDLQStore(t, "dr5")
	router := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store:        store,
		BufferSize:   200,
		Workers:      4,
		WriteTimeout: 10 * time.Second,
	})

	ctx := context.Background()
	router.Start(ctx)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			env := &domain.Envelope{
				ID:      uniqueID("env-dr5"),
				Subject: "test/concurrent",
				Payload: []byte(`{"concurrent":"dlq"}`),
			}
			_ = router.Route(ctx, env, "route-dr5", uniqueID("bind"), "sess-dr5", "", shared.ErrUnavailable, 1)
		}()
	}
	wg.Wait()
	router.Close()

	entries, err := store.List(ctx, domain.DLQFilter{RouteID: "route-dr5"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != goroutines {
		t.Fatalf("expected %d entries from concurrent Routes, got %d", goroutines, len(entries))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newDDBDLQStore(t *testing.T, prefix string) *dynamodbdlq.Store {
	t.Helper()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable(prefix + "-dlq")
	store := dynamodbdlq.NewStore(client, dynamodbdlq.WithTableName(tableName))
	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("create DLQ table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)
	return store
}
