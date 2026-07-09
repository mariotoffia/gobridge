package dynamodblease_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Compile-time assertion that *Store satisfies the port it adapts. It lives in
// the external test package because the store adapter layer may not depend on
// ports (the production package satisfies ports.LeaseStore structurally).
var _ ports.LeaseStore = (*dynamodblease.Store)(nil)

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

		// Takeover of an actively-held lease uses a local-clock observation
		// window (finding M5): the taker must first observe the lease, then wait
		// at least the lease TTL before it may seize. A single post-expiry
		// acquire is therefore rejected; the pre-expiry attempt establishes the
		// observation baseline.
		if _, err := store.Acquire(ctx, "em-exp", "bridge-2", 30*time.Second, nil); err == nil {
			t.Fatal("takeover before the observation window elapsed must be rejected")
		}

		time.Sleep(2 * time.Second) // OTHER: real elapsed time required — emulator observation window keys on wall-clock renewed_at (M5)

		tok2, err := store.Acquire(ctx, "em-exp", "bridge-2", 30*time.Second, nil)
		if err != nil {
			t.Fatalf("expired takeover: %v", err)
		}
		if tok2.Version <= tok.Version {
			t.Fatalf("version must increase after expired takeover: v1=%d, v2=%d",
				tok.Version, tok2.Version)
		}
	})

	t.Run("ObservationWindowAbortsOnRenewal", func(t *testing.T) {
		// Finding M5: the takeover observation window keys on the liveness tuple
		// (owner, version, renewed_at). If the owner renews after the taker
		// begins observing, renewed_at advances, the tuple changes, and the
		// observation window RESETS — the taker must NOT seize an actively
		// renewing lease even after a full TTL of local time has passed. This is
		// exactly the skew-immunity property: no wall-clock comparison is made,
		// so a fast-clocked taker cannot steal a live lease.
		tokA, err := store.Acquire(ctx, "em-obs", "bridge-A", 1*time.Second, nil)
		if err != nil {
			t.Fatalf("owner-A acquire: %v", err)
		}

		// Taker begins observing (baseline established).
		if _, err := store.Acquire(ctx, "em-obs", "bridge-B", 30*time.Second, nil); err == nil {
			t.Fatal("first takeover attempt must be rejected (establishes observation)")
		}

		// Owner renews: renewed_at advances (version stays the same). The short
		// pause guarantees the renewed_at epoch-millis differs from the value the
		// taker recorded at baseline, so the tuple change is observable.
		time.Sleep(50 * time.Millisecond) // OTHER: real elapsed time — renewed_at epoch-millis must differ from baseline
		if _, err := store.Renew(ctx, "em-obs", tokA, 1*time.Second, nil); err != nil {
			t.Fatalf("owner-A renew: %v", err)
		}

		// Wait past the original TTL. In the no-renewal case (see
		// ExpiredTakeoverIncrementsVersion) the taker would now seize; here the
		// advanced renewed_at changes the tuple and resets the window, so the
		// taker must NOT seize the still-recently-renewed lease.
		time.Sleep(2 * time.Second) // OTHER: real elapsed time required — full TTL must pass to prove the window reset

		if _, err := store.Acquire(ctx, "em-obs", "bridge-B", 30*time.Second, nil); err == nil {
			t.Fatal("takeover must be aborted after the owner renewed within the window")
		}
	})
}
