package bootstrap

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// TestBaselineHash_MatchesFileContentHash verifies the poll watcher's baseline
// is seeded from the sha256 of the exact bytes on disk -- the same value the
// watcher's own fileHash computes -- so an edit landing between the initial
// Load and Watch is emitted rather than absorbed into a Watch-time re-read.
func TestBaselineHash_MatchesFileContentHash(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	require.NoError(t, parser.WriteFile(cfgPath, &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-a",
			DeploymentMode: "standalone",
			LogLevel:       "info",
		},
	}))

	got, ok := baselineHash(t.Context(), cfgPath, newDefaultPluginRegistry())
	require.True(t, ok, "a valid config file must yield a baseline hash")

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256(data), got,
		"baseline must equal the sha256 of the file bytes the watcher compares against")
}

// TestBaselineHash_MissingFileHasNoBaseline verifies a missing config file
// records no baseline, so the watcher keeps its disk-read baseline (unchanged
// behavior) and the optionalFileSource fallback path is untouched.
func TestBaselineHash_MissingFileHasNoBaseline(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	_, ok := baselineHash(t.Context(), cfgPath, newDefaultPluginRegistry())
	assert.False(t, ok, "a missing file must not produce a baseline")
}

// TestBaselineHash_UnparseableFileHasNoBaseline verifies a file that fails to
// parse records no baseline (LoadHash advances only after a successful parse),
// so a corrupt file at startup cannot pin a bogus baseline into the watcher.
func TestBaselineHash_UnparseableFileHasNoBaseline(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	// Unclosed flow mapping -> YAML syntax error -> parse fails.
	require.NoError(t, os.WriteFile(cfgPath, []byte("bridge: {id: 'x'"), 0o600))

	_, ok := baselineHash(t.Context(), cfgPath, newDefaultPluginRegistry())
	assert.False(t, ok, "an unparseable file must not produce a baseline")
}
