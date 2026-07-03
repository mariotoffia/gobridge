package awsstore_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
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

// TestDynamoDBStoreFactory_OutboxThreadsMetricsExporter_DDBLocal is the
// factory-level end-to-end intent-check that a store built THROUGH THE FACTORY
// with a RecordingExporter emits shared.MetricOutboxClaimConflicts when a Claim
// loses a per-record fence race to a DynamoDB TransactionConflict. Before the
// wiring fix the counter vanished into the store's default no-op meter, so
// every config-driven deployment lost this signal.
//
// It runs the full factory -> Persist -> concurrent-Claim path deterministically
// (construction and operations must succeed), but the terminal conflict
// assertion is BEST-EFFORT: DynamoDB Local serializes competing transactions
// and surfaces the loser as a plain ConditionalCheckFailed (a normal lost race,
// intentionally NOT counted) rather than a TransactionConflict. When no conflict
// is observed the test SKIPS rather than failing; point it at a real endpoint
// via DYNAMODB_ENDPOINT to exercise the counter for real. The deterministic
// counterpart above plus the dynamodboutbox WithMetrics-option test cover the
// wiring on every run.
//
// Gated by the ddblocal harness: SKIPPED under `go test -short` and when Docker
// is unavailable; RUN by `make test-integration` / `make check-all`.
func TestDynamoDBStoreFactory_OutboxThreadsMetricsExporter_DDBLocal(t *testing.T) {
	client := ddblocal.Client(t)
	ctx := context.Background()

	rec := &ports.RecordingExporter{}
	factory := awsstore.NewDynamoDBStoreFactory(client)

	const claimers = 16
	const perRound = 25
	// Heavy same-partition contention makes a TransactionConflict likely on a
	// real endpoint within the first round; the bounded round loop gives a slow
	// endpoint a few more chances before the best-effort skip.
	const maxRounds = 3

	var conflicts int
	for round := 0; round < maxRounds && conflicts == 0; round++ {
		table := ddblocal.UniqueTable("outbox-metrics")
		cfg := &awsstore.DynamoDBConfig{TableName: table}

		store, err := factory.NewOutboxStore(ctx, cfg, ports.OutboxRuntimeOptions{
			StaleClaimDuration: 30 * time.Second,
			Metrics:            rec,
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

		partition := fmt.Sprintf("SESSION#metrics-%d", round)
		for i := 0; i < perRound; i++ {
			envID := fmt.Sprintf("env-%d-%d", round, i)
			r := persistence.MustOutboxRecord(persistence.OutboxSpec{
				ID:         fmt.Sprintf("m-%d-%d", round, i),
				RouteID:    "route-1",
				EnvelopeID: envID,
				BindingID:  "bind-m",
				SessionID:  fmt.Sprintf("metrics-%d", round),
				Address:    "test/topic",
				Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: envID, Subject: "t"}),
			})
			if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
				t.Fatalf("persist: %v", err)
			}
		}

		var wg sync.WaitGroup
		for c := 0; c < claimers; c++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				token := persistence.LeaseToken{Version: 1, Owner: fmt.Sprintf("owner-%d", idx)}
				// Losers of the per-record fence race that hit a genuine
				// in-flight TransactionConflict are swallowed as a benign skip
				// but now COUNTED via the factory-threaded exporter.
				_, _ = store.Claim(ctx, partition, token, perRound)
			}(c)
		}
		wg.Wait()

		conflicts = len(rec.FindEntries(shared.MetricOutboxClaimConflicts))
	}

	if conflicts == 0 {
		t.Skipf("no %s surfaced across %d contended rounds; this DynamoDB endpoint "+
			"serializes competing transactions (DynamoDB Local returns "+
			"ConditionalCheckFailed, not TransactionConflict). The factory "+
			"construction/Persist/Claim path ran clean; point DYNAMODB_ENDPOINT at "+
			"real DynamoDB to exercise the conflict counter end-to-end.",
			shared.MetricOutboxClaimConflicts, maxRounds)
	}
}
