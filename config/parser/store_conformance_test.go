package parser_test

import (
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/configstoretest"
)

// TestFileStoreConformance verifies the ConfigStore contract on temporary files.
func TestFileStoreConformance(t *testing.T) {
	configstoretest.Run(t, func(t *testing.T) ports.ConfigStore {
		t.Helper()
		return &parser.FileStore{
			Path:     filepath.Join(t.TempDir(), "config.yaml"),
			Registry: ports.NewRegistry(),
		}
	})
}
