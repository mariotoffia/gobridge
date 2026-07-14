package sqlitemanagedsubscriptions_test

import (
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

func TestStoreConformance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-subscriptions.db")
	open := func(t *testing.T) ports.ManagedSubscriptionStore {
		t.Helper()
		store, err := sqlitemanagedsubscriptions.NewStore(path)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	storetest.RunManagedSubscriptionStoreTests(t, storetest.ManagedSubscriptionStoreHarness{
		Store: open(t), Restart: open,
	})
}
