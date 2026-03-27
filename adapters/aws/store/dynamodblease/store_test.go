package dynamodblease_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

// Verifies the DynamoDB lease store passes the shared lease store conformance suite.
func TestConformanceSuite(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("leases-conf")
	store := dynamodblease.NewStore(client, dynamodblease.WithTableName(tableName))

	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	storetest.RunLeaseStoreTests(t, store, &storetest.LeaseTestOptions{
		LeaseTTL: 2 * time.Second,
	})
}

// Verifies DynamoDB-specific lease store behavior including idempotent EnsureTable, renew/release cycles, and expired takeover versioning.
func TestDynamoDBSpecificErrorMapping(t *testing.T) {
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable("leases-errmap")
	store := dynamodblease.NewStore(client, dynamodblease.WithTableName(tableName))
	ctx := context.Background()

	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)

	t.Run("EnsureTableIdempotent", func(t *testing.T) {
		if err := store.EnsureTable(ctx); err != nil {
			t.Fatalf("second EnsureTable should be idempotent: %v", err)
		}
	})

	t.Run("AcquireRenewReleaseCycle", func(t *testing.T) {
		tok, err := store.Acquire(ctx, "em-cycle", "bridge-1", 30*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if tok.Version != 1 {
			t.Fatalf("fresh acquire version: got %d, want 1", tok.Version)
		}

		renewed, err := store.Renew(ctx, "em-cycle", tok, 30*time.Second, nil)
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		if renewed.Version != tok.Version {
			t.Fatalf("renew should keep version: got %d, want %d", renewed.Version, tok.Version)
		}

		if err := store.Release(ctx, "em-cycle", tok); err != nil {
			t.Fatalf("release: %v", err)
		}

		tok2, err := store.Acquire(ctx, "em-cycle", "bridge-2", 30*time.Second, nil)
		if err != nil {
			t.Fatalf("re-acquire after release: %v", err)
		}
		if tok2.Version <= tok.Version {
			t.Fatalf("re-acquire version must increase: got %d, want > %d", tok2.Version, tok.Version)
		}
	})

	t.Run("ExpiredTakeoverIncrementsVersion", func(t *testing.T) {
		tok, err := store.Acquire(ctx, "em-exp", "bridge-1", 1*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}

		time.Sleep(2 * time.Second)

		tok2, err := store.Acquire(ctx, "em-exp", "bridge-2", 30*time.Second, nil)
		if err != nil {
			t.Fatalf("expired takeover: %v", err)
		}
		if tok2.Version <= tok.Version {
			t.Fatalf("version must increase after expired takeover: v1=%d, v2=%d",
				tok.Version, tok2.Version)
		}
	})
}
