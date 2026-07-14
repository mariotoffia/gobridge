package sqlitemanagedsubscriptions_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

func TestNewStoreContextCreatesOwnerOnlyDatabaseAndSidecars(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX file permission test")
	}
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	for _, mask := range []int{0, 0o077} {
		t.Run(fmt.Sprintf("umask-%03o", mask), func(t *testing.T) {
			syscall.Umask(mask)
			path := filepath.Join(t.TempDir(), "adapter-owned", "managed.db")
			store, err := sqlitemanagedsubscriptions.NewStoreContext(context.Background(), path)
			if err != nil {
				t.Fatalf("NewStoreContext: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			assertMode(t, filepath.Dir(path), 0o700)
			assertMode(t, path, 0o600)
			assertMode(t, path+"-wal", 0o600)
			assertMode(t, path+"-shm", 0o600)
		})
	}
}

func TestNewStoreContextRejectsInsecureExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	store, err := sqlitemanagedsubscriptions.NewStoreContext(context.Background(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("insecure existing database returned a store")
	}
	if err == nil {
		t.Fatal("insecure existing database must be rejected")
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("operator-owned file was silently chmodded to %04o", got)
	}
}

func TestNewStoreContextRejectsDatabaseSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "managed.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	store, err := sqlitemanagedsubscriptions.NewStoreContext(context.Background(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("database symlink returned a store")
	}
	if err == nil {
		t.Fatal("database symlink must be rejected")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%q) = %04o, want %04o", path, got, want)
	}
}
