package docsexamples_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// `bridge.shutdown_timeout` is the whole process budget on SIGTERM: the config
// watcher join, the rollout drive stop, the HTTP shutdown, the runtime drain,
// store close and telemetry flush all run inside it. `drain_timeout` bounds only
// the runtime drain within that budget, and `max_drain_timeout` bounds one
// outbox drain batch. An example that sets a drain budget equal to the process
// budget therefore leaves zero headroom for everything after the drain: the
// process is still closing stores when the orchestrator's SIGKILL lands.
//
// Every published YAML example that states both is checked, so a copied config
// can never ship the zero-headroom shape.
//
// Category: unit (TESTS.md §1) — the Markdown files are the fixtures.

var (
	shutdownBudgetLine = regexp.MustCompile(`^\s*shutdown_timeout:\s*"?([0-9a-z.]+)"?`)
	drainBudgetLine    = regexp.MustCompile(`^\s*(drain_timeout|max_drain_timeout):\s*"?([0-9a-z.]+)"?`)
)

func TestShutdownExamples_DrainLeavesProcessHeadroom(t *testing.T) {
	root := repoRoot(t)
	pages := append(markdownFiles(t, root), "ARCHITECTURE.md")

	checked := 0
	var violations []string
	for _, page := range pages {
		src, err := os.ReadFile(filepath.Join(root, page))
		require.NoError(t, err)
		for _, block := range extractYAMLBlocks(string(src)) {
			var shutdown time.Duration
			drains := map[string]time.Duration{}
			for line := range strings.SplitSeq(block.body, "\n") {
				if m := shutdownBudgetLine.FindStringSubmatch(line); m != nil {
					d, err := time.ParseDuration(m[1])
					require.NoError(t, err, "%s:%d: unparseable shutdown_timeout %q", page, block.line, m[1])
					shutdown = d
				}
				if m := drainBudgetLine.FindStringSubmatch(line); m != nil {
					d, err := time.ParseDuration(m[2])
					require.NoError(t, err, "%s:%d: unparseable %s %q", page, block.line, m[1], m[2])
					drains[m[1]] = d
				}
			}
			if shutdown == 0 || len(drains) == 0 {
				continue
			}
			checked++
			for key, d := range drains {
				if d >= shutdown {
					violations = append(violations, fmt.Sprintf("%s:%d: %s %s is not below shutdown_timeout %s",
						page, block.line, key, d, shutdown))
				}
			}
		}
	}
	require.NotZero(t, checked, "no YAML example states both a shutdown and a drain budget — the fixtures moved")
	require.Empty(t, violations, "published examples whose drain budget consumes the whole process budget")
}
