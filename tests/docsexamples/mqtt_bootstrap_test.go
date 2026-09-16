package docsexamples_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMQTTScenarioBootstrap_ActualWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the published Go bootstrap with the local toolchain")
	}
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs/scenarios/24-mqtt-mixed-qos-to-sqs.md"))
	require.NoError(t, err)
	_, goFence, found := strings.Cut(string(doc), "```go\n")
	require.True(t, found)
	source, _, found := strings.Cut(goFence, "\n```")
	require.True(t, found)
	_, yamlFence, found := strings.Cut(string(doc), "```yaml\n")
	require.True(t, found)
	blueprint, _, found := strings.Cut(yamlFence, "\n```")
	require.True(t, found)

	// Execute the published parser, registration, Build and Stop unchanged.
	// Substitute only connection startup/wait: this is a construction proof,
	// not a connection to the documentation's example broker.
	require.Equal(t, 1, strings.Count(source, "rt.Start(ctx)"))
	require.Equal(t, 1, strings.Count(source, "<-ctx.Done()"))
	source = strings.Replace(source, "rt.Start(ctx)", "rt.ValidateRoutes()", 1)
	source = strings.Replace(source, "<-ctx.Done()", "", 1)
	for _, tc := range []struct {
		name, remove, replacement string
	}{
		{name: "published"},
		{name: "missing native decoder", remove: "nativestore.Register(reg)", replacement: "nil"},
		{name: "missing native factory", remove: `RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory()).`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			for _, name := range []string{"managed", "deadletters"} {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o700))
			}
			configPath := filepath.Join(dir, "bridge.yaml")
			yaml := strings.ReplaceAll(blueprint, "/var/lib/gobridge", filepath.ToSlash(dir))
			require.NoError(t, os.WriteFile(configPath, []byte(yaml), 0o600))
			variant := strings.Replace(source, `"bridge.yaml"`, strconv.Quote(configPath), 1)
			require.NotEqual(t, source, variant)
			if tc.remove != "" {
				require.Equal(t, 1, strings.Count(variant, tc.remove))
				variant = strings.Replace(variant, tc.remove, tc.replacement, 1)
			}
			mainPath := filepath.Join(dir, "main.go")
			testPath := filepath.Join(dir, "bootstrap_test.go")
			require.NoError(t, os.WriteFile(mainPath, []byte(variant), 0o600))
			require.NoError(t, os.WriteFile(testPath, []byte(`package main
import "testing"
func TestPublishedBootstrap(t *testing.T) {
	if err := run(t.Context()); err != nil {
		t.Fatalf("published bootstrap: %v", err)
	}
}
`), 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			t.Cleanup(cancel)
			cmd := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "-timeout=30s",
				"-run=^TestPublishedBootstrap$", mainPath, testPath)
			output, runErr := cmd.CombinedOutput()
			require.NoError(t, ctx.Err(), "bootstrap command timed out")
			if tc.remove == "" {
				require.NoError(t, runErr, "%s", output)
				return
			}
			require.Error(t, runErr, "removing required registration must break the actual bootstrap")
			assert.Contains(t, string(output), "published bootstrap:", "failure must come from the bootstrap, not compilation")
			assert.Contains(t, string(output), "sqlite")
		})
	}
}
