package dynamodboutbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

func newTestStore(t *testing.T) *dynamodboutbox.Store {
	t.Helper()

	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("outbox")
	store := dynamodboutbox.NewStore(client,
		dynamodboutbox.WithTableName(tableName),
		dynamodboutbox.WithStaleClaimDuration(0), // immediate reclaim for tests
	)

	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	return store
}

// --- Conformance Suite ---

func TestOutboxStoreConformance(t *testing.T) {
	store := newTestStore(t)
	storetest.RunOutboxStoreTests(t, store)
}

// --- DynamoDB-Specific Tests ---

func TestIdempotentPersistAfterRedelivery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r1 := domain.OutboxRecord{
		ID:         "redeliv-1",
		RouteID:    "route-1",
		EnvelopeID: "env-redeliv",
		BindingID:  "bind-redeliv",
		SessionID:  "sess-redeliv",
		Address:    "test/topic",
		Envelope:   domain.Envelope{ID: "env-redeliv", Subject: "test", Payload: []byte("first")},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r1}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := domain.OutboxRecord{
		ID:         "redeliv-2",
		RouteID:    "route-1",
		EnvelopeID: "env-redeliv",
		BindingID:  "bind-redeliv",
		SessionID:  "sess-redeliv",
		Address:    "test/topic",
		Envelope:   domain.Envelope{ID: "env-redeliv", Subject: "test", Payload: []byte("redelivered")},
	}
	err := store.Persist(ctx, []domain.OutboxRecord{r2})
	if !errors.Is(err, domain.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord on redelivery, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-redeliv", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly 1 record (no duplicate), got %d", len(pending))
	}
	if string(pending[0].Envelope.Payload) != "first" {
		t.Fatalf("original record should be preserved, got payload %q", pending[0].Envelope.Payload)
	}
}

func TestFanOutAtomicity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	records := []domain.OutboxRecord{
		{
			ID: "fo-atom-1", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-A",
			SessionID: "sess-fo-atom", Address: "topic/a",
			Envelope: domain.Envelope{ID: "env-fo-atom", Subject: "test"},
		},
		{
			ID: "fo-atom-2", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-B",
			SessionID: "sess-fo-atom", Address: "topic/b",
			Envelope: domain.Envelope{ID: "env-fo-atom", Subject: "test"},
		},
		{
			ID: "fo-atom-3", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-C",
			SessionID: "sess-fo-atom", Address: "topic/c",
			Envelope: domain.Envelope{ID: "env-fo-atom", Subject: "test"},
		},
	}

	if err := store.Persist(ctx, records); err != nil {
		t.Fatalf("fan-out persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-fo-atom", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 fan-out records, got %d", len(pending))
	}
}

func TestFanOutDuplicateRejection(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := []domain.OutboxRecord{
		{
			ID: "fo-dup-1", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-X",
			SessionID: "sess-fo-dup", Address: "topic/x",
			Envelope: domain.Envelope{ID: "env-fo-dup", Subject: "test"},
		},
	}
	if err := store.Persist(ctx, first); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	second := []domain.OutboxRecord{
		{
			ID: "fo-dup-2", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-X",
			SessionID: "sess-fo-dup", Address: "topic/x",
			Envelope: domain.Envelope{ID: "env-fo-dup", Subject: "test"},
		},
		{
			ID: "fo-dup-3", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-Y",
			SessionID: "sess-fo-dup", Address: "topic/y",
			Envelope: domain.Envelope{ID: "env-fo-dup", Subject: "test"},
		},
	}
	err := store.Persist(ctx, second)
	if !errors.Is(err, domain.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord on fan-out with existing binding, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-fo-dup", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 record (transaction rolled back), got %d", len(pending))
	}
}

func TestConcurrentClaimSafety(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := domain.OutboxRecord{
		ID: "conc-1", RouteID: "route-1",
		EnvelopeID: "env-conc", BindingID: "bind-conc",
		SessionID: "sess-conc", Address: "test/topic",
		Envelope: domain.Envelope{ID: "env-conc", Subject: "test", Payload: []byte("concurrent")},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var wg sync.WaitGroup
	results := make([][]domain.OutboxRecord, 3)
	errs := make([]error, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token := domain.LeaseToken{Version: uint64(idx + 1), Owner: fmt.Sprintf("owner-%d", idx)}
			results[idx], errs[idx] = store.Claim(ctx,
				"SESSION#sess-conc", fmt.Sprintf("owner-%d", idx), token, 10)
		}(i)
	}
	wg.Wait()

	totalClaimed := 0
	for i := 0; i < 3; i++ {
		if errs[i] != nil {
			t.Fatalf("claim %d error: %v", i, errs[i])
		}
		totalClaimed += len(results[i])
	}

	if totalClaimed < 1 {
		t.Fatal("expected at least 1 successful claim across concurrent attempts")
	}
}

func TestReplayCountIncrementsOnReclaim(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := domain.OutboxRecord{
		ID: "replay-1", RouteID: "route-1",
		EnvelopeID: "env-replay", BindingID: "bind-replay",
		SessionID: "sess-replay", Address: "test/topic",
		Envelope: domain.Envelope{ID: "env-replay", Subject: "test"},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token1 := domain.LeaseToken{Version: 1, Owner: "owner-1"}
	claimed1, err := store.Claim(ctx, "SESSION#sess-replay", "owner-1", token1, 10)
	if err != nil || len(claimed1) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed1))
	}
	if claimed1[0].ReplayCount != 1 {
		t.Fatalf("first claim replay_count: got %d, want 1", claimed1[0].ReplayCount)
	}

	time.Sleep(10 * time.Millisecond)

	token2 := domain.LeaseToken{Version: 2, Owner: "owner-2"}
	claimed2, err := store.Claim(ctx, "SESSION#sess-replay", "owner-2", token2, 10)
	if err != nil || len(claimed2) != 1 {
		t.Fatalf("reclaim: err=%v, len=%d", err, len(claimed2))
	}
	if claimed2[0].ReplayCount != 2 {
		t.Fatalf("reclaim replay_count: got %d, want 2", claimed2[0].ReplayCount)
	}
}

func TestCompleteWithStaleTokenRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := domain.OutboxRecord{
		ID: "stale-1", RouteID: "route-1",
		EnvelopeID: "env-stale", BindingID: "bind-stale",
		SessionID: "sess-stale", Address: "test/topic",
		Envelope: domain.Envelope{ID: "env-stale", Subject: "test"},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := domain.LeaseToken{Version: 5, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-stale", "owner-A", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	staleToken := domain.LeaseToken{Version: 3, Owner: "owner-A"}
	err := store.Complete(ctx, []string{"stale-1"}, staleToken)
	if !errors.Is(err, domain.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken with stale version, got %v", err)
	}
}

func TestExpireWithNoExpirySetSkips(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := domain.OutboxRecord{
		ID: "noexp-1", RouteID: "route-1",
		EnvelopeID: "env-noexp", BindingID: "bind-noexp",
		SessionID: "sess-noexp", Address: "test/topic",
		Envelope: domain.Envelope{ID: "env-noexp", Subject: "test"},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	n, err := store.Expire(ctx, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 expired (no expiry set), got %d", n)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-noexp", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
}

func TestDispatchHeadersRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := domain.OutboxRecord{
		ID: "hdr-ddb-1", RouteID: "route-1",
		EnvelopeID: "env-hdr-ddb", BindingID: "bind-hdr-ddb",
		SessionID: "sess-hdr-ddb", Address: "test/topic",
		Envelope: domain.Envelope{ID: "env-hdr-ddb", Subject: "test"},
		DispatchHeaders: map[string]any{
			"x-custom":  "value",
			"x-numeric": float64(42),
		},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-hdr-ddb", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if v, ok := pending[0].DispatchHeaders["x-custom"]; !ok || v != "value" {
		t.Fatalf("dispatch header x-custom: %v", pending[0].DispatchHeaders)
	}
}

func TestEnvelopePayloadRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	payload := []byte(`{"device":"sensor-1","temp":22.5}`)
	r := domain.OutboxRecord{
		ID: "pay-1", RouteID: "route-1",
		EnvelopeID: "env-pay", BindingID: "bind-pay",
		SessionID: "sess-pay", Address: "devices/sensor-1/temp",
		Envelope: domain.Envelope{
			ID:      "env-pay",
			Subject: "temperature",
			Payload: payload,
			Headers: map[string]any{
				domain.HeaderContentType:   "application/json",
				domain.HeaderCorrelationID: "corr-123",
			},
		},
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-pay", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if string(pending[0].Envelope.Payload) != string(payload) {
		t.Fatalf("payload mismatch: got %q", pending[0].Envelope.Payload)
	}
	if pending[0].Envelope.Subject != "temperature" {
		t.Fatalf("subject: got %q", pending[0].Envelope.Subject)
	}
	ct, _ := domain.GetHeaderString(pending[0].Envelope.Headers, domain.HeaderContentType)
	if ct != "application/json" {
		t.Fatalf("content-type header: got %q", ct)
	}
}

func TestCreateTableIdempotent(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("outbox-idem")
	store := dynamodboutbox.NewStore(client, dynamodboutbox.WithTableName(tableName))

	ctx := context.Background()
	if err := store.CreateTable(ctx); err != nil {
		t.Fatalf("first CreateTable: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	if err := store.CreateTable(ctx); err != nil {
		t.Fatalf("second CreateTable should be idempotent, got: %v", err)
	}
}

func TestFullLifecycleWithExpiry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	expiry := time.Now().Add(-30 * time.Minute).Truncate(time.Millisecond)
	r := domain.OutboxRecord{
		ID: "lce-1", RouteID: "route-1",
		EnvelopeID: "env-lce", BindingID: "bind-lce",
		SessionID: "sess-lce", Address: "test/topic",
		Envelope:  domain.Envelope{ID: "env-lce", Subject: "test"},
		ExpiresAt: expiry,
	}
	if err := store.Persist(ctx, []domain.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-lce", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	n, err := store.Expire(ctx, time.Now())
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired, got %d", n)
	}

	pending, err = store.QueryPending(ctx, "SESSION#sess-lce", 10)
	if err != nil {
		t.Fatalf("query after expire: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after expire, got %d", len(pending))
	}
}
