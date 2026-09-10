package docsexamples_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLocalDeploySelector_RejectsEmptyRuns verifies a parent alone cannot satisfy a child selector.
func TestLocalDeploySelector_RejectsEmptyRuns(t *testing.T) {
	var guard string
	for _, line := range strings.Split(makefileRecipe(t, "test-local-deploy"), "\n") {
		if strings.Contains(line, "No local deployment tests matched the selector") {
			guard = strings.TrimSuffix(strings.TrimSpace(line), `\`)
			break
		}
	}
	require.NotEmpty(t, guard)
	for _, tc := range []struct {
		name, output string
		reject       bool
	}{
		{"matched", "=== RUN   TestLocal_Proof\n--- PASS: TestLocal_Proof (0.01s)\nPASS\n", false},
		{"missing parent", "testing: warning: no tests to run\nPASS\n", true},
		{"missing child", "=== RUN   TestLocal_Proof\n--- PASS: TestLocal_Proof (0.00s)\ntesting: warning: no tests to run\nPASS\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "reports"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "reports/test-local-deploy.log"), []byte(tc.output), 0o600))
			cmd := exec.CommandContext(t.Context(), "bash", "-c", `rc=0; `+guard+` exit "$rc"`)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if tc.reject {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit, string(out))
				require.Equal(t, 1, exit.ExitCode())
			} else {
				require.NoError(t, err, string(out))
			}
		})
	}
}
