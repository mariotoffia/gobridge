//go:build integration_local

package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// localRunDirectories creates the host directories the containers mount.
//
// They have to be reachable from the Docker daemon as a bind source AND from the
// test process, so they live under the OS temp directory, which Docker shares by
// default on every platform the suite runs on.
func localRunDirectories(t *testing.T, state *localBackend) {
	t.Helper()
	state.runDir = filepath.Join(os.TempDir(), state.network)
	state.configDir = filepath.Join(state.runDir, "config")
	if err := os.MkdirAll(state.configDir, 0o777); err != nil {
		t.Fatalf("create shared config directory: %v", err)
	}
	// Each per-stack mount gets the runtime user's ownership before deployment.
	// MkdirAll honours the umask, so the shared parent mode is set explicitly.
	if err := os.Chmod(state.configDir, 0o777); err != nil {
		t.Fatalf("open shared config directory: %v", err)
	}
}
