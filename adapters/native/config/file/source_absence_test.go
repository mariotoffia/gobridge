package file

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestSourceAbsenceRequiresExistingParentAndNoEntry(t *testing.T) {
	for _, kind := range []string{"absent", "parent absent", "symlink", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			switch kind {
			case "parent absent":
				path = filepath.Join(path, "config.yaml")
			case "symlink":
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte("["), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewSource(path, newTestRegistry(t)).Load(t.Context())
			if err == nil || errors.Is(err, shared.ErrNotFound) != (kind == "absent") {
				t.Fatalf("classification for %s: %v", kind, err)
			}
		})
	}
}
