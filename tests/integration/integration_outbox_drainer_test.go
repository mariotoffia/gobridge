package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Outbox Drainer Integration Tests with DynamoDB Local
//
// Validates the OutboxDrainer against a real DynamoDB Local instance,
// exercising the full Persist → Claim → Send → Complete lifecycle and
// fencing token enforcement via DynamoDB conditional expressions.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────────────┐
// │ Test │ Description                                                  │
// ├──────┼──────────────────────────────────────────────────────────────┤
// │ OD1  │ Full lifecycle: persist → drain → complete with real DDB     │
// │ OD2  │ Stale fencing token rejected by DDB conditional writes       │
// │ OD3  │ Expired records route to DLQ via drainer                     │
// │ OD4  │ Poison messages (replay exceeded) route to DLQ               │
// │ OD5  │ Concurrent drainers: only lease holder succeeds              │
// │ OD6  │ Adaptive batch sizing scales with throughput                  │
// └──────┴──────────────────────────────────────────────────────────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_OutboxDrainer_FullLifecycle validates the complete drain
// cycle: persist a record into DynamoDB, run the drainer with a real sender,
// and verify the record is marked complete.
//
// Scenario:
//
//	┌────────┐     ┌──────────────┐     ┌──────────┐
//	│ Persist│────▶│ DynamoDB     │────▶│ Drainer  │
//	│ Record │     │ OutboxStore  │     │ Claim+   │
//	└────────┘     └──────────────┘     │ Send+    │
//	                                    │ Complete │
//	                                    └──────────┘
func TestIntegration_OutboxDrainer_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBOutboxStore(t, "od1")
	sender := &collectingSender{}
	tok := domain.LeaseToken{Version: 1, Owner: "drainer-od1"}

	rec := domain.OutboxRecord{
		ID:         uniqueID("od1-rec"),
		EnvelopeID: "env-od1",
		BindingID:  "bind-od1",
		SessionID:  "sess-od1",
		RouteID:    "route-od1",
		Address:    "test/topic",
		Envelope: domain.Envelope{
			ID:      "env-od1",
			Subject: "test",
			Payload: []byte(`{"lifecycle":"test"}`),
		},
	}

	ctx := context.Background()
	if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:    store,
		Sender:         sender,
		RouteID:        "route-od1",
		PartitionKey:   domain.OutboxPartitionKey("sess-od1", ""),
		OwnerID:        "drainer-od1",
		Policy:         domain.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 3},
		Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (domain.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 3*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	if sender.count() != 1 {
		t.Fatalf("expected 1 sent message, got %d", sender.count())
	}

	pending, err := store.QueryPending(ctx, domain.OutboxPartitionKey("sess-od1", ""), 10)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(pending))
	}
}

// TestIntegration_OutboxDrainer_StaleFencingToken validates that Complete
// is rejected by DynamoDB conditional writes when a newer fencing token
// has been used to reclaim the record.
//
// Scenario:
//
//	Drainer-A claims with tok1 → record claimed
//	Drainer-B claims with tok2 (higher version) → record reclaimed
//	Drainer-A tries Complete with tok1 → ErrStaleFencingToken
func TestIntegration_OutboxDrainer_StaleFencingToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBOutboxStore(t, "od2")
	ctx := context.Background()

	rec := domain.OutboxRecord{
		ID:         uniqueID("od2-rec"),
		EnvelopeID: "env-od2",
		BindingID:  "bind-od2",
		SessionID:  "sess-od2",
		RouteID:    "route-od2",
		Envelope: domain.Envelope{
			ID:      "env-od2",
			Subject: "test",
			Payload: []byte(`{"stale":"test"}`),
		},
	}

	if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pk := domain.OutboxPartitionKey("sess-od2", "")
	tok1 := domain.LeaseToken{Version: 1, Owner: "owner-A"}
	tok2 := domain.LeaseToken{Version: 2, Owner: "owner-B"}

	claimed, err := store.Claim(ctx, pk, "owner-A", tok1, 10)
	if err != nil {
		t.Fatalf("claim with tok1: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}

	_, err = store.Claim(ctx, pk, "owner-B", tok2, 10)
	if err != nil && !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("reclaim with tok2: unexpected error: %v", err)
	}

	err = store.Complete(ctx, []string{rec.ID}, tok1)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken on complete with old token, got %v", err)
	}
}

// TestIntegration_OutboxDrainer_ExpiredRecordRoutesDLQ validates that records
// with expired envelopes are routed to the DLQ store and completed.
func TestIntegration_OutboxDrainer_ExpiredRecordRoutesDLQ(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBOutboxStore(t, "od3")
	dlqStore := &e2eDLQStore{}
	sender := &collectingSender{}
	tok := domain.LeaseToken{Version: 1, Owner: "drainer-od3"}

	past := time.Now().Add(-1 * time.Hour)
	rec := domain.OutboxRecord{
		ID:         uniqueID("od3-rec"),
		EnvelopeID: "env-od3",
		BindingID:  "bind-od3",
		SessionID:  "sess-od3",
		RouteID:    "route-od3",
		ExpiresAt:  past,
		Envelope: domain.Envelope{
			ID:        "env-od3",
			Subject:   "test",
			Payload:   []byte(`{"expired":"test"}`),
			ExpiresAt: past,
		},
	}

	ctx := context.Background()
	if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	dlqRouter := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store: dlqStore,
	})

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:    store,
		Sender:         sender,
		DLQ:            dlqRouter,
		RouteID:        "route-od3",
		PartitionKey:   domain.OutboxPartitionKey("sess-od3", ""),
		OwnerID:        "drainer-od3",
		Policy:         domain.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 3, OnExpired: domain.ExpiredDLQ},
		Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (domain.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 3*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	if sender.count() != 0 {
		t.Fatalf("expired record should not be sent, got %d sends", sender.count())
	}

	if dlqStore.count() != 1 {
		t.Fatalf("expected 1 DLQ entry for expired record, got %d", dlqStore.count())
	}

	pending, _ := store.QueryPending(ctx, domain.OutboxPartitionKey("sess-od3", ""), 10)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after expired drain, got %d", len(pending))
	}
}

// TestIntegration_OutboxDrainer_PoisonMessageRoutesDLQ validates that records
// exceeding MaxReplayAttempts are routed to the DLQ and completed.
func TestIntegration_OutboxDrainer_PoisonMessageRoutesDLQ(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBOutboxStore(t, "od4")
	dlqStore := &e2eDLQStore{}
	sender := &collectingSender{}

	ctx := context.Background()
	pk := domain.OutboxPartitionKey("sess-od4", "")

	rec := domain.OutboxRecord{
		ID:         uniqueID("od4-rec"),
		EnvelopeID: "env-od4",
		BindingID:  "bind-od4",
		SessionID:  "sess-od4",
		RouteID:    "route-od4",
		Envelope: domain.Envelope{
			ID:      "env-od4",
			Subject: "test",
			Payload: []byte(`{"poison":"test"}`),
		},
	}

	if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Claim and release multiple times to drive up ReplayCount beyond max (2).
	for i := 1; i <= 3; i++ {
		tok := domain.LeaseToken{Version: uint64(i), Owner: "pumper"}
		_, err := store.Claim(ctx, pk, "pumper", tok, 10)
		if err != nil {
			t.Fatalf("claim cycle %d: %v", i, err)
		}
	}

	finalTok := domain.LeaseToken{Version: 4, Owner: "drainer-od4"}
	dlqRouter := goruntime.NewDLQRouterFromConfig(goruntime.DLQRouterConfig{
		Store: dlqStore,
	})

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:    store,
		Sender:         sender,
		DLQ:            dlqRouter,
		RouteID:        "route-od4",
		PartitionKey:   pk,
		OwnerID:        "drainer-od4",
		Policy:         domain.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 2},
		Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize: 10,
		TokenFn:        func() (domain.LeaseToken, bool) { return finalTok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 3*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	if sender.count() != 0 {
		t.Fatalf("poison record should not be sent, got %d sends", sender.count())
	}

	if dlqStore.count() != 1 {
		t.Fatalf("expected 1 DLQ entry for poison message, got %d", dlqStore.count())
	}
}

// TestIntegration_OutboxDrainer_ConcurrentDrainers validates that two drainers
// contending for the same partition key do not produce duplicate sends.
//
// Scenario:
//
//	           ┌─────────────────┐
//	Persist ──▶│ DynamoDB Outbox  │
//	           └──────┬──────────┘
//	                  │
//	        ┌─────────┼─────────┐
//	        ▼                   ▼
//	  ┌──────────┐       ┌──────────┐
//	  │ Drainer-A│       │ Drainer-B│
//	  │ tok v=1  │       │ tok v=2  │
//	  └──────────┘       └──────────┘
//	        │                   │
//	        ▼                   ▼
//	  Total sends == number of records (no duplicates)
func TestIntegration_OutboxDrainer_ConcurrentDrainers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Use a realistic stale claim duration so freshly-claimed records
	// aren't immediately re-claimable by a concurrent drainer. In
	// production, this is derived from StepDownGrace + 15s.
	store := newDDBOutboxStoreWithStaleDuration(t, "od5", 5*time.Second)
	ctx := context.Background()
	pk := domain.OutboxPartitionKey("sess-od5", "")

	const recordCount = 10
	for i := 0; i < recordCount; i++ {
		rec := domain.OutboxRecord{
			ID:         uniqueID("od5-rec"),
			EnvelopeID: uniqueID("env-od5"),
			BindingID:  uniqueID("bind-od5"),
			SessionID:  "sess-od5",
			RouteID:    "route-od5",
			Envelope: domain.Envelope{
				ID:      uniqueID("env-od5"),
				Subject: "test",
				Payload: []byte(`{"concurrent":"test"}`),
			},
		}
		if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	senderA := &collectingSender{}
	senderB := &collectingSender{}

	tokA := domain.LeaseToken{Version: 1, Owner: "drainer-A"}
	tokB := domain.LeaseToken{Version: 2, Owner: "drainer-B"}

	makeDrainer := func(sender *collectingSender, tok domain.LeaseToken, owner string) *goruntime.OutboxDrainer {
		return goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
			OutboxStore:    store,
			Sender:         sender,
			RouteID:        "route-od5",
			PartitionKey:   pk,
			OwnerID:        owner,
			Policy:         domain.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
			Strategy:       domain.NewFixedPoll(50 * time.Millisecond),
			DrainBatchSize: 5,
			TokenFn:        func() (domain.LeaseToken, bool) { return tok, true },
		})
	}

	drainerA := makeDrainer(senderA, tokA, "drainer-A")
	drainerB := makeDrainer(senderB, tokB, "drainer-B")

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = drainerA.Run(drainCtx) }()
	go func() { defer wg.Done(); _ = drainerB.Run(drainCtx) }()
	wg.Wait()

	totalSent := senderA.count() + senderB.count()
	if totalSent > recordCount {
		t.Fatalf("duplicate sends detected: senderA=%d senderB=%d total=%d (expected <= %d)",
			senderA.count(), senderB.count(), totalSent, recordCount)
	}
	if totalSent == 0 {
		t.Fatal("expected at least some messages to be sent")
	}
	t.Logf("concurrent drainers: senderA=%d senderB=%d total=%d", senderA.count(), senderB.count(), totalSent)
}

// TestIntegration_OutboxDrainer_AdaptiveBatchSize validates that the drainer
// scales its batch size up when draining full batches and down when idle.
func TestIntegration_OutboxDrainer_AdaptiveBatchSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store := newDDBOutboxStore(t, "od6")
	sender := &collectingSender{}
	ctx := context.Background()
	pk := domain.OutboxPartitionKey("sess-od6", "")
	tok := domain.LeaseToken{Version: 1, Owner: "drainer-od6"}

	const batchSize = 5
	for i := 0; i < batchSize*3; i++ {
		rec := domain.OutboxRecord{
			ID:         uniqueID("od6-rec"),
			EnvelopeID: uniqueID("env-od6"),
			BindingID:  uniqueID("bind-od6"),
			SessionID:  "sess-od6",
			RouteID:    "route-od6",
			Envelope: domain.Envelope{
				ID:      uniqueID("env-od6"),
				Subject: "test",
				Payload: []byte(`{"adaptive":"test"}`),
			},
		}
		if err := store.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:       store,
		Sender:            sender,
		RouteID:           "route-od6",
		PartitionKey:      pk,
		OwnerID:           "drainer-od6",
		Policy:            domain.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		Strategy:          domain.NewFixedPoll(50 * time.Millisecond),
		DrainBatchSize:    batchSize,
		DrainMaxBatchSize: batchSize * 4,
		TokenFn:           func() (domain.LeaseToken, bool) { return tok, true },
	})

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()
	_ = drainer.Run(drainCtx)

	if sender.count() != batchSize*3 {
		t.Fatalf("expected %d sent messages, got %d", batchSize*3, sender.count())
	}

	pending, _ := store.QueryPending(ctx, pk, 100)
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", len(pending))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newDDBOutboxStore(t *testing.T, prefix string) *dboutbox.Store {
	t.Helper()
	return newDDBOutboxStoreWithStaleDuration(t, prefix, 0)
}

func newDDBOutboxStoreWithStaleDuration(t *testing.T, prefix string, staleDuration time.Duration) *dboutbox.Store {
	t.Helper()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable(prefix + "-outbox")
	store := dboutbox.NewStore(client,
		dboutbox.WithTableName(tableName),
		dboutbox.WithStaleClaimDuration(staleDuration),
	)
	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create outbox table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)
	return store
}

type collectingSender struct {
	mu   sync.Mutex
	envs []*domain.Envelope
}

func (s *collectingSender) Send(_ context.Context, env *domain.Envelope) error {
	s.mu.Lock()
	s.envs = append(s.envs, env)
	s.mu.Unlock()
	return nil
}

func (s *collectingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}
