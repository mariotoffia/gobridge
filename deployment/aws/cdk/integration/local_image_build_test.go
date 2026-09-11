//go:build integration_local

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeBuild_EmbedsStagedConfigAndCleansInput(t *testing.T) {
	payload := []byte("bridge:\n  id: staged-deployment\n  description: " + strings.Repeat("x", 200*1024) + "\n")
	record := installImageBuildCLI(t, initialConfigDigest(payload))
	state := &localBackend{network: localRunPrefix + "unit"}
	var staged string
	t.Run("build", func(t *testing.T) {
		t.Setenv(localImageEnv, "")
		image := buildLocalRuntimeImage(t, state, localRuntimeAsset{
			ID: "asset", Platform: "linux/arm64", Config: payload,
		})
		require.Equal(t, []string{image}, state.runtimeImages, "the run owns cleanup of its built image")
		data, err := os.ReadFile(record)
		require.NoError(t, err)
		args := strings.Split(strings.TrimSpace(string(data)), "\n")
		require.Equal(t, []string{"build", "--platform", "linux/arm64", "--build-arg"}, args[:4])
		relative := strings.TrimPrefix(args[4], "INITIAL_CONFIG_FILE=")
		require.True(t, strings.HasPrefix(relative, ".gobridge-initial-"))
		require.Equal(t, filepath.Base(relative), relative, "input must be inside the root Docker context")
		root, err := filepath.Abs("../../../..")
		require.NoError(t, err)
		require.Equal(t, []string{"-t", image, root}, args[5:])
		staged = filepath.Join(root, relative)
		data, err = os.ReadFile(staged)
		require.NoError(t, err)
		require.Equal(t, payload, data, "build input must be the CDK-staged document, not a fixture reconstruction")
	})
	_, err := os.Stat(staged)
	require.ErrorIs(t, err, os.ErrNotExist, "input cleanup must run even though image cleanup waits for suite shutdown")
}

func TestLocalRuntimeBuild_UsesVerifiedOverrideWithoutRebuilding(t *testing.T) {
	payload := []byte("bridge:\n  id: override\n")
	record := installImageBuildCLI(t, initialConfigDigest(payload))
	t.Setenv(localImageEnv, " example.com/prebuilt:exact-config ")
	state := &localBackend{network: localRunPrefix + "unit"}
	image := buildLocalRuntimeImage(t, state, localRuntimeAsset{Platform: "linux/amd64", Config: payload})
	require.Equal(t, "example.com/prebuilt:exact-config", image)
	require.Empty(t, state.runtimeImages, "an override belongs to the caller and must not be removed")
	_, err := os.Stat(record)
	require.ErrorIs(t, err, os.ErrNotExist, "a supplied override must not silently trigger a different build")
}

// This CLI stand-in records only build arguments. It never starts a container
// or writes to the runtime config target; deployment tests own that proof.
func installImageBuildCLI(t *testing.T, digest string) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "build-args")
	script := `#!/bin/sh
set -eu
case "$1" in
build) printf '%s\n' "$@" >"$GOBRIDGE_TEST_BUILD_ARGS" ;;
run)
  test "$2" = --rm
  test "$3" = --network
  test "$4" = none
  test "$8" = -initial-config-digest
  printf '%s\n' "$GOBRIDGE_TEST_CONFIG_DIGEST"
  ;;
*) exit 2 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOBRIDGE_TEST_BUILD_ARGS", record)
	t.Setenv("GOBRIDGE_TEST_CONFIG_DIGEST", digest)
	return record
}
