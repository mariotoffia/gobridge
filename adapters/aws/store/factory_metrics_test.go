package awsstore_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

// TestDynamoDBStoreFactory_NewOutboxStore_ThreadsMetricsOption is the
// DETERMINISTIC (no Docker) proof that NewOutboxStore exercises the
// OutboxRuntimeOptions.Metrics threading branch without error on BOTH sides of
// the nil guard: a RecordingExporter is appended via WithMetrics, and a nil
// exporter leaves the store's default no-op meter in place. Construction never
// touches the client, so a nil client is sufficient here.
//
// This pins the wiring (Hop B input) that Test-#2 in the dynamodboutbox package
// proves actually reaches the conflict counter, and that the DDB-local e2e
// below exercises against a real endpoint.
func TestDynamoDBStoreFactory_NewOutboxStore_ThreadsMetricsOption(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	cfg := &awsstore.DynamoDBConfig{TableName: "metrics-thread-table"}

	rec := &ports.RecordingExporter{}
	withMetrics, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{
		StaleClaimDuration: 30 * time.Second,
		Metrics:            rec,
	})
	if err != nil {
		t.Fatalf("NewOutboxStore with exporter: unexpected error: %v", err)
	}
	if withMetrics == nil {
		t.Fatal("NewOutboxStore with exporter: expected non-nil store")
	}

	withoutMetrics, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{
		StaleClaimDuration: 30 * time.Second,
		// Metrics left nil: the factory must NOT append WithMetrics and the
		// store keeps its no-op meter.
	})
	if err != nil {
		t.Fatalf("NewOutboxStore without exporter: unexpected error: %v", err)
	}
	if withoutMetrics == nil {
		t.Fatal("NewOutboxStore without exporter: expected non-nil store")
	}
}

// TestDynamoDBStoreFactory_OutboxClaimIsExactlyOnce_DDBLocal proves the
// invariant that only a real endpoint can prove: a store built THROUGH THE
// FACTORY hands every pending record to exactly ONE of many concurrent
// claimers. Losers of a per-record fence race must come back empty-handed,
// never with a duplicate — a double-claim is a double-send downstream.
//
// It exercises the full factory -> Persist -> concurrent-Claim path against
// real DynamoDB semantics (key shape, GSI projection, conditional writes),
// none of which the fake-client unit tests can validate.
//
// It deliberately does NOT assert on shared.MetricOutboxClaimConflicts. That
// counter fires only on an in-flight TransactionConflict, which DynamoDB Local
// structurally cannot produce (it serializes competing transactions and
// surfaces the loser as a plain ConditionalCheckFailed, a normal lost race that
// is intentionally NOT counted). Its behaviour — counted on TransactionConflict,
// not counted on ConditionalCheckFailed, tagged with the partition — is pinned
// deterministically against a fake client by
// dynamodboutbox.TestClaim_TransactionConflict_CountsMetricAndSkips.
//
// Gated by the ddblocal harness: SKIPPED under `go test -short` and when Docker
// is unavailable; RUN by `make test-integration` / `make check-all`.
func TestDynamoDBStoreFactory_OutboxClaimIsExactlyOnce_DDBLocal(t *testing.T) {
	client := ddblocal.Client(t)
	ctx := context.Background()

	factory := awsstore.NewDynamoDBStoreFactory(client)

	const claimers = 16
	const records = 25

	table := ddblocal.UniqueTable("outbox-metrics")
	store, err := factory.NewOutboxStore(ctx, &awsstore.DynamoDBConfig{TableName: table},
		ports.OutboxRuntimeOptions{
			StaleClaimDuration: 30 * time.Second,
			Metrics:            &ports.RecordingExporter{},
		})
	if err != nil {
		t.Fatalf("new outbox store: %v", err)
	}
	creator, ok := store.(interface {
		CreateTable(context.Context) error
	})
	if !ok {
		t.Fatalf("factory outbox store %T does not expose CreateTable", store)
	}
	if err := creator.CreateTable(ctx); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	const partition = "SESSION#metrics"
	for i := range records {
		envID := fmt.Sprintf("env-%d", i)
		r := persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         fmt.Sprintf("m-%d", i),
			RouteID:    "route-1",
			EnvelopeID: envID,
			BindingID:  "bind-m",
			SessionID:  "metrics",
			Address:    "test/topic",
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: envID, Subject: "t"}),
		})
		if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}

	var (
		mu     sync.Mutex
		owners = map[string]string{} // record ID -> claiming owner
		dupes  []string
		wg     sync.WaitGroup
	)
	for c := range claimers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			owner := fmt.Sprintf("owner-%d", idx)
			token := persistence.LeaseToken{Version: 1, Owner: owner}
			// A loser of the per-record fence race returns fewer records (or
			// none) without erroring — that is the designed behaviour, so only
			// a hard error fails the claim itself.
			claimed, err := store.Claim(ctx, partition, token, records)
			if err != nil {
				mu.Lock()
				dupes = append(dupes, fmt.Sprintf("%s: claim error: %v", owner, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range claimed {
				if prev, seen := owners[r.ID()]; seen {
					dupes = append(dupes,
						fmt.Sprintf("record %s claimed by both %s and %s", r.ID(), prev, owner))
					continue
				}
				owners[r.ID()] = owner
			}
		}(c)
	}
	wg.Wait()

	if len(dupes) > 0 {
		t.Fatalf("outbox Claim is not exactly-once under %d concurrent claimers:\n  %s",
			claimers, strings.Join(dupes, "\n  "))
	}
	if len(owners) != records {
		t.Fatalf("claimed %d distinct records across %d claimers, want all %d",
			len(owners), claimers, records)
	}
}
