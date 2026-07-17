//go:build windows

package sqlitemanagedsubscriptions_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestNewStoreContextFailsExplicitlyWhenSecureWindowsSemanticsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.db")
	store, err := sqlitemanagedsubscriptions.NewStoreContext(t.Context(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("Windows secure-store construction returned a store")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("Windows secure-store error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "secure no-follow") || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("Windows secure-store error is not explicit: %v", err)
	}
}
