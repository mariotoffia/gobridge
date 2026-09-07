package parser_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestFileStoreSaveRejectsInvalidStoredVersion verifies version counters cannot wrap.
func TestFileStoreSaveRejectsInvalidStoredVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
	}{
		{"negative", -1},
		{"exhausted", math.MaxInt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &parser.FileStore{
				Path: filepath.Join(t.TempDir(), "config.yaml"), Registry: ports.NewRegistry(),
			}
			original := &ports.BridgeConfig{Version: tc.version, Bridge: ports.BridgeSettings{ID: "original"}}
			require.NoError(t, parser.WriteFile(store.Path, original))
			cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "replacement"}}

			assert.ErrorIs(t, store.Save(t.Context(), cfg), shared.ErrInvalidConfig)
			assert.Zero(t, cfg.Version)
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, original, got)
		})
	}
}

// TestFileStoreSaveReadFailurePreservesDocument verifies corrupt files are not overwritten.
func TestFileStoreSaveReadFailurePreservesDocument(t *testing.T) {
	for _, raw := range []string{"[", "-0.5", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			store := &parser.FileStore{
				Path: filepath.Join(t.TempDir(), "config.yaml"), Registry: ports.NewRegistry(),
			}
			original := []byte("version: " + raw)
			require.NoError(t, os.WriteFile(store.Path, original, 0o600))
			cfg := &ports.BridgeConfig{Version: 42, Bridge: ports.BridgeSettings{ID: "replacement"}}

			require.Error(t, store.Save(t.Context(), cfg))
			assert.Equal(t, 42, cfg.Version)
			got, err := os.ReadFile(store.Path)
			require.NoError(t, err)
			assert.Equal(t, original, got)
		})
	}
}

// TestFileStoreSaveWriteFailurePreservesVersion verifies failed writes do not look committed.
func TestFileStoreSaveWriteFailurePreservesVersion(t *testing.T) {
	store := &parser.FileStore{
		Path: filepath.Join(t.TempDir(), "missing", "config.yaml"), Registry: ports.NewRegistry(),
	}
	cfg := &ports.BridgeConfig{Version: 42, Bridge: ports.BridgeSettings{ID: "replacement"}}
	require.Error(t, store.Save(t.Context(), cfg))
	assert.Equal(t, 42, cfg.Version)
}
