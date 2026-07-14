//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

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
	path := filepath.Join(t.TempDir(), "adapter-owned", "managed-subscriptions.db")
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
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod temp root: %v", err)
	}
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	for _, mask := range []int{0, 0o077} {
		t.Run(fmt.Sprintf("umask-%03o", mask), func(t *testing.T) {
			syscall.Umask(mask)
			path := filepath.Join(root, fmt.Sprintf("mask-%03o", mask), "managed.db")
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
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	path := filepath.Join(parent, "managed.db")
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
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	target := filepath.Join(parent, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	path := filepath.Join(parent, "managed.db")
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

func TestNewStoreContextRejectsNonCanonicalFilesystemPaths(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "managed.db")
	for _, path := range []string{
		":memory:",
		"managed.db",
		"file:" + absolute,
		"file:" + absolute + "?mode=rwc",
		absolute + "?mode=rwc",
		absolute + "#fragment",
		filepath.Dir(absolute) + "/nested/../managed.db",
	} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			store, err := sqlitemanagedsubscriptions.NewStoreContext(t.Context(), path)
			if store != nil {
				_ = store.Close()
				t.Fatal("unsafe/non-canonical path returned a store")
			}
			if err == nil {
				t.Fatal("unsafe/non-canonical path must be rejected")
			}
		})
	}
}

func TestNewStoreContextRejectsSymlinkedOrUnsafeParentChain(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o777); err != nil {
		t.Fatalf("Mkdir unsafe: %v", err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatalf("Chmod unsafe: %v", err)
	}

	for _, path := range []string{
		filepath.Join(linkedParent, "managed.db"),
		filepath.Join(unsafeParent, "managed.db"),
	} {
		store, err := sqlitemanagedsubscriptions.NewStoreContext(t.Context(), path)
		if store != nil {
			_ = store.Close()
			t.Fatalf("unsafe parent path %q returned a store", path)
		}
		if err == nil {
			t.Fatalf("unsafe parent path %q must be rejected", path)
		}
	}
}

func TestNewStoreContextRejectsPreexistingSidecarSymlinks(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			parent := t.TempDir()
			if err := os.Chmod(parent, 0o700); err != nil {
				t.Fatalf("Chmod parent: %v", err)
			}
			path := filepath.Join(parent, "managed.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("WriteFile db: %v", err)
			}
			target := filepath.Join(parent, "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatalf("WriteFile target: %v", err)
			}
			if err := os.Symlink(target, path+suffix); err != nil {
				t.Fatalf("Symlink sidecar: %v", err)
			}
			store, err := sqlitemanagedsubscriptions.NewStoreContext(t.Context(), path)
			if store != nil {
				_ = store.Close()
				t.Fatal("sidecar symlink returned a store")
			}
			if err == nil {
				t.Fatal("sidecar symlink must be rejected")
			}
		})
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
