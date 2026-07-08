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
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Compile-time assertion that the DynamoDB store implements the optional
// OutboxReleaser capability the drainer type-asserts for the transient-
// failure fast path. It lives in the test file because the production
// package satisfies its ports structurally (no ports import).
var _ ports.OutboxReleaser = (*dynamodboutbox.Store)(nil)

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
		// Version-only reclaim: stale-claim recovery is pinned separately in
		// TestOutboxStaleReclaimConformance with a fake clock.
		dynamodboutbox.WithStaleClaimDuration(0),
	)

	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	return store
}

// --- Conformance Suite ---

// Verifies the DynamoDB outbox store passes the shared outbox store conformance suite.
func TestOutboxStoreConformance(t *testing.T) {
	store := newTestStore(t)
	storetest.RunOutboxStoreTests(t, store)
}

// Verifies the optional fast-release capability against the shared
// conformance suite so release fencing matches memory/SQLite.
func TestOutboxReleaseConformance(t *testing.T) {
	store := newTestStore(t)
	storetest.RunOutboxReleaseTests(t, store)
}

// Validates the wall-clock stale-claim reclaim behaviour against the shared
// conformance suite, driven by a fake clock (TESTS.md: no time.Sleep).
func TestOutboxStaleReclaimConformance(t *testing.T) {
	const stale = 5 * time.Minute

	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("outbox")
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := dynamodboutbox.NewStore(client,
		dynamodboutbox.WithTableName(tableName),
		dynamodboutbox.WithClock(clk),
		dynamodboutbox.WithStaleClaimDuration(stale),
	)
	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	storetest.RunOutboxStaleReclaimTests(t, store, stale, clk.Advance)
}

// Validates the replay-budget first-attempt contract against the shared
// conformance suite, driven by a fake clock (TESTS.md: no time.Sleep). Proves
// the claimOne if_not_exists stamp and the marshal/unmarshal round-trip.
func TestOutboxFirstAttemptConformance(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("outbox")
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	store := dynamodboutbox.NewStore(client,
		dynamodboutbox.WithTableName(tableName),
		dynamodboutbox.WithClock(clk),
	)
	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	storetest.RunOutboxFirstAttemptTests(t, store, clk.Advance)
}

// --- DynamoDB-Specific Tests ---

// Verifies duplicate persist for the same envelope ID returns ErrDuplicateRecord without altering the first record.
func TestIdempotentPersistAfterRedelivery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r1 := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "redeliv-1",
		RouteID:    "route-1",
		EnvelopeID: "env-redeliv",
		BindingID:  "bind-redeliv",
		SessionID:  "sess-redeliv",
		Address:    "test/topic",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-redeliv", Subject: "test", Payload: []byte("first")}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r1}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	r2 := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID:         "redeliv-2",
		RouteID:    "route-1",
		EnvelopeID: "env-redeliv",
		BindingID:  "bind-redeliv",
		SessionID:  "sess-redeliv",
		Address:    "test/topic",
		Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-redeliv", Subject: "test", Payload: []byte("redelivered")}),
	})
	err := store.Persist(ctx, []*persistence.OutboxRecord{r2})
	if !errors.Is(err, shared.ErrDuplicateRecord) {
		t.Fatalf("expected ErrDuplicateRecord on redelivery, got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-redeliv", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly 1 record (no duplicate), got %d", len(pending))
	}
	if string(pending[0].Snapshot().Payload()) != "first" {
		t.Fatalf("original record should be preserved, got payload %q", pending[0].Snapshot().Payload())
	}
}

// Verifies a single Persist call can atomically write multiple fan-out records for one session.
func TestFanOutAtomicity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	records := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-atom-1", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-A",
			SessionID: "sess-fo-atom", Address: "topic/a",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-atom", Subject: "test"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-atom-2", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-B",
			SessionID: "sess-fo-atom", Address: "topic/b",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-atom", Subject: "test"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-atom-3", RouteID: "route-1",
			EnvelopeID: "env-fo-atom", BindingID: "bind-C",
			SessionID: "sess-fo-atom", Address: "topic/c",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-atom", Subject: "test"}),
		})}

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

// Verifies the per-record Persist idempotency contract on fan-out: a batch
// that partially overlaps already-persisted records persists the NEW legs
// and silently skips the existing ones (no error, no rollback). This is
// what makes fan-out re-persist after a partial failure safe.
func TestFanOutPartialOverlapPersistsNewLegs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-dup-1", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-X",
			SessionID: "sess-fo-dup", Address: "topic/x",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-dup", Subject: "test"}),
		})}
	if err := store.Persist(ctx, first); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	second := []*persistence.OutboxRecord{
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-dup-2", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-X",
			SessionID: "sess-fo-dup", Address: "topic/x",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-dup", Subject: "test"}),
		}),
		persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID: "fo-dup-3", RouteID: "route-1",
			EnvelopeID: "env-fo-dup", BindingID: "bind-Y",
			SessionID: "sess-fo-dup", Address: "topic/y",
			Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-fo-dup", Subject: "test"}),
		})}
	if err := store.Persist(ctx, second); err != nil {
		t.Fatalf("partial-overlap persist must succeed (new leg persisted), got %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-fo-dup", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 records (original + new leg, duplicate skipped), got %d", len(pending))
	}
	if string(pending[0].ID()) != "fo-dup-1" {
		t.Fatalf("original record must be preserved, got first ID %q", pending[0].ID())
	}
}

// Verifies concurrent Claim calls do not corrupt state and at least one succeeds.
func TestConcurrentClaimSafety(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "conc-1", RouteID: "route-1",
		EnvelopeID: "env-conc", BindingID: "bind-conc",
		SessionID: "sess-conc", Address: "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-conc", Subject: "test", Payload: []byte("concurrent")}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var wg sync.WaitGroup
	results := make([][]*persistence.OutboxRecord, 3)
	errs := make([]error, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token := persistence.LeaseToken{Version: uint64(idx + 1), Owner: fmt.Sprintf("owner-%d", idx)}
			results[idx], errs[idx] = store.Claim(ctx,
				"SESSION#sess-conc", token, 10)
		}(i)
	}
	wg.Wait()

	totalClaimed := 0
	for i := 0; i < 3; i++ {
		if errs[i] != nil {
			// A concurrently raised fence legitimately preempts a
			// lower-version claimer (ports.OutboxStore contract): the
			// preempted claimer gets ErrStaleFencingToken, never records.
			if errors.Is(errs[i], shared.ErrStaleFencingToken) {
				continue
			}
			t.Fatalf("claim %d error: %v", i, errs[i])
		}
		totalClaimed += len(results[i])
	}

	if totalClaimed < 1 {
		t.Fatal("expected at least 1 successful claim across concurrent attempts")
	}
}

// Verifies ReplayCount increments when a session is claimed again after a prior claim.
func TestReplayCountIncrementsOnReclaim(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "replay-1", RouteID: "route-1",
		EnvelopeID: "env-replay", BindingID: "bind-replay",
		SessionID: "sess-replay", Address: "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-replay", Subject: "test"}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token1 := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	claimed1, err := store.Claim(ctx, "SESSION#sess-replay", token1, 10)
	if err != nil || len(claimed1) != 1 {
		t.Fatalf("first claim: err=%v, len=%d", err, len(claimed1))
	}
	if claimed1[0].ReplayCount() != 1 {
		t.Fatalf("first claim replay_count: got %d, want 1", claimed1[0].ReplayCount())
	}

	// Version-monotonic preemption: token2 (v2) reclaims the record claimed
	// at v1 without any wall-clock wait (TESTS.md: no time.Sleep).
	token2 := persistence.LeaseToken{Version: 2, Owner: "owner-2"}
	claimed2, err := store.Claim(ctx, "SESSION#sess-replay", token2, 10)
	if err != nil || len(claimed2) != 1 {
		t.Fatalf("reclaim: err=%v, len=%d", err, len(claimed2))
	}
	if claimed2[0].ReplayCount() != 2 {
		t.Fatalf("reclaim replay_count: got %d, want 2", claimed2[0].ReplayCount())
	}
}

// Verifies Complete with a stale fencing token returns ErrStaleFencingToken.
func TestCompleteWithStaleTokenRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "stale-1", RouteID: "route-1",
		EnvelopeID: "env-stale", BindingID: "bind-stale",
		SessionID: "sess-stale", Address: "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-stale", Subject: "test"}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	token := persistence.LeaseToken{Version: 5, Owner: "owner-A"}
	if _, err := store.Claim(ctx, "SESSION#sess-stale", token, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	staleToken := persistence.LeaseToken{Version: 3, Owner: "owner-A"}
	err := store.Complete(ctx, []string{"stale-1"}, staleToken)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken with stale version, got %v", err)
	}
}

// Verifies Expire does not remove records that have no expiry timestamp set.
func TestExpireWithNoExpirySetSkips(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "noexp-1", RouteID: "route-1",
		EnvelopeID: "env-noexp", BindingID: "bind-noexp",
		SessionID: "sess-noexp", Address: "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-noexp", Subject: "test"}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	n, err := store.Expire(ctx, time.Now().Add(1*time.Hour), "SESSION#sess-noexp")
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

// Verifies DispatchHeaders persist and round-trip through QueryPending.
func TestDispatchHeadersRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "hdr-ddb-1", RouteID: "route-1",
		EnvelopeID: "env-hdr-ddb", BindingID: "bind-hdr-ddb",
		SessionID: "sess-hdr-ddb", Address: "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-hdr-ddb", Subject: "test"}),
		DispatchHeaders: map[string]any{
			"x-custom":  "value",
			"x-numeric": float64(42),
		},
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-hdr-ddb", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	if v, ok := pending[0].DispatchHeaders()["x-custom"]; !ok || v != "value" {
		t.Fatalf("dispatch header x-custom: %v", pending[0].DispatchHeaders())
	}
}

// Verifies envelope payload and standard headers round-trip through persist and query.
func TestEnvelopePayloadRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	payload := []byte(`{"device":"sensor-1","temp":22.5}`)
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "pay-1", RouteID: "route-1",
		EnvelopeID: "env-pay", BindingID: "bind-pay",
		SessionID: "sess-pay", Address: "devices/sensor-1/temp",
		Envelope: *messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
			ID:      "env-pay",
			Subject: "temperature",
			Payload: payload,
			Headers: map[string]any{
				messaging.HeaderContentType:   "application/json",
				messaging.HeaderCorrelationID: "corr-123",
			},
		}),
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-pay", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1, got %d", len(pending))
	}
	env := pending[0].Snapshot()
	if string(env.Payload()) != string(payload) {
		t.Fatalf("payload mismatch: got %q", env.Payload())
	}
	if env.Subject() != "temperature" {
		t.Fatalf("subject: got %q", env.Subject())
	}
	ct, _ := messaging.GetHeaderString(env.Headers(), messaging.HeaderContentType)
	if ct != "application/json" {
		t.Fatalf("content-type header: got %q", ct)
	}
}

// Verifies CreateTable succeeds when invoked repeatedly for the same store.
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

// Verifies expired outbox records are removed by Expire and no longer appear as pending.
func TestFullLifecycleWithExpiry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	expiry := time.Now().Add(-30 * time.Minute).Truncate(time.Millisecond)
	r := persistence.MustOutboxRecord(persistence.OutboxSpec{
		ID: "lce-1", RouteID: "route-1",
		EnvelopeID: "env-lce", BindingID: "bind-lce",
		SessionID: "sess-lce", Address: "test/topic",
		Envelope:  *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-lce", Subject: "test"}),
		ExpiresAt: expiry,
	})
	if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pending, err := store.QueryPending(ctx, "SESSION#sess-lce", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	n, err := store.Expire(ctx, time.Now(), "SESSION#sess-lce")
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
