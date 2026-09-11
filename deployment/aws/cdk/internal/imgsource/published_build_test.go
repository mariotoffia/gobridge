package imgsource

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

// TestGoBuild_PublishedModule verifies the actual rendered build commands
// against a local module proxy, without network access or a repository checkout.
func TestGoBuild_PublishedModule(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a published-module fixture with Go")
	}
	root := t.TempDir()
	proxy := filepath.Join(root, "proxy")
	versionDir := filepath.Join(proxy, "example.com", "bridge", "@v")
	require.NoError(t, os.MkdirAll(versionDir, 0o700))
	const module = "module example.com/bridge\n\ngo 1.25.0\n"
	for name, content := range map[string]string{
		"list":        "v1.0.0\n",
		"v1.0.0.mod":  module,
		"v1.0.0.info": `{"Version":"v1.0.0","Time":"2000-01-01T00:00:00Z"}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(versionDir, name), []byte(content), 0o600))
	}
	program, err := os.ReadFile(filepath.Join("testdata", "initial_config_main.go"))
	require.NoError(t, err)
	archive, err := os.Create(filepath.Join(versionDir, "v1.0.0.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = archive.Close() })
	writer := zip.NewWriter(archive)
	for name, content := range map[string][]byte{
		"go.mod":                           []byte(module),
		"cmd/custom/main.go":               program,
		"cmd/custom/initial-config.base64": nil,
	} {
		entry, err := writer.Create("example.com/bridge@v1.0.0/" + name)
		require.NoError(t, err)
		_, err = entry.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, archive.Close())

	dockerfile, initial := renderBuildContext(GoBuildProps{
		Package: "example.com/bridge/cmd/custom", Version: "v1.0.0",
	}, &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "published"}})
	require.NotNil(t, initial)
	build := filepath.Join(root, "build")
	require.NoError(t, os.Mkdir(build, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(build, initial.name), initial.data, 0o600))
	_, run, found := strings.Cut(dockerfile, "\nRUN ")
	require.True(t, found)
	run, _, found = strings.Cut(run, "\n\nFROM ")
	require.True(t, found)
	run = strings.ReplaceAll(run, "/build", build)
	run = strings.ReplaceAll(run, "/gobridge-aws", filepath.Join(root, "binary"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euc", run)
	cmd.Dir = build
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off", "GOFLAGS=-modcacherw", "GOTOOLCHAIN=local",
		"GOPROXY=file://"+proxy, "GOSUMDB=off", "GOMODCACHE="+filepath.Join(root, "modules"))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.FileExists(t, filepath.Join(root, "binary"))
}
