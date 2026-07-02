package dynamodboutbox_test

// This file holds the J-N7 integration coverage: concurrent claimers racing
// the cold-partition fence seed against a real DynamoDB (DynamoDB Local via
// the shared ddblocal harness), closing the residual gap that J2/J3/J5/J6
// were only unit-tested against fakes.
//
// Like the rest of this package's store tests it is gated by the ddblocal
// harness: it is SKIPPED under `go test -short` (and when Docker is absent)
// and RUN by `make test-integration` / `make check-all` (which run without
// -short and with Docker). It therefore needs no build tag; run it directly
// with:
//
//	go test -run TestConcurrentClaimersColdSeedRace ./adapters/aws/store/dynamodboutbox/...
//
// or point it at an existing endpoint via DYNAMODB_ENDPOINT.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Verifies that many owners claiming a freshly-persisted (cold, no FENCE row)
// partition concurrently all hit the seed==0 cold path at once (J-N6) without
// (1) handing any record to two owners (J-N7 double-claim) or (2) corrupting
// the monotonic fence — which must end up persisted so a later lower-version
// claim is rejected O(1) instead of triggering another bounded scan.
func TestConcurrentClaimersColdSeedRace(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("outbox-coldseed")
	// Positive stale-claim: a record claimed by one owner is NOT immediately
	// reclaimable by another at the same fence version, so each pending record
	// is won by exactly one owner and double-claims are unambiguous.
	store := dynamodboutbox.NewStore(client,
		dynamodboutbox.WithTableName(table),
		dynamodboutbox.WithStaleClaimDuration(30*time.Second),
	)
	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	ctx := context.Background()

	const numRecords = 25
	const partition = "SESSION#coldseed"
	for i := 0; i < numRecords; i++ {
		r := persistence.MustOutboxRecord(persistence.OutboxSpec{
			ID:         fmt.Sprintf("cs-%d", i),
			RouteID:    "route-1",
			EnvelopeID: fmt.Sprintf("env-cs-%d", i),
			BindingID:  "bind-cs",
			SessionID:  "coldseed",
			Address:    "test/topic",
			Envelope:   *messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-cs-%d", i), Subject: "t"}),
		})
		// Persist one-by-one so each record is an independent pending row on a
		// partition that still has NO fence row.
		if err := store.Persist(ctx, []*persistence.OutboxRecord{r}); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}

	// Fire many claimers concurrently at the SAME fence version but distinct
	// owners, so they all race the cold seed==0 path simultaneously.
	const claimers = 8
	var wg sync.WaitGroup
	claimedBy := make([]map[string]bool, claimers)
	errs := make([]error, claimers)
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token := persistence.LeaseToken{Version: 1, Owner: fmt.Sprintf("owner-%d", idx)}
			claimed, err := store.Claim(ctx, partition, token, numRecords)
			if err != nil {
				errs[idx] = err
				return
			}
			m := make(map[string]bool, len(claimed))
			for _, rec := range claimed {
				m[rec.ID()] = true
			}
			claimedBy[idx] = m
		}(c)
	}
	wg.Wait()

	seen := make(map[string]int) // recordID -> owner index that claimed it
	for c := 0; c < claimers; c++ {
		if errs[c] != nil {
			t.Fatalf("claimer %d error: %v", c, errs[c])
		}
		for id := range claimedBy[c] {
			if prev, dup := seen[id]; dup {
				t.Fatalf("record %s double-claimed by owner-%d and owner-%d (cold-seed race corruption)", id, prev, c)
			}
			seen[id] = c
		}
	}
	if len(seen) != numRecords {
		t.Fatalf("expected all %d records claimed exactly once across owners, got %d", numRecords, len(seen))
	}

	// The concurrent seed race must have persisted a monotonic fence (>=1), so
	// a stale lower-version claim is now rejected without re-scanning.
	_, err := store.Claim(ctx, partition, persistence.LeaseToken{Version: 0, Owner: "stale"}, numRecords)
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("expected ErrStaleFencingToken after fence seeded by the race, got %v", err)
	}
}
