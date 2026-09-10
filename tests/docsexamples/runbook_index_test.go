package docsexamples_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A runbook nobody can find is a runbook nobody reads. The index is the only
// entry point — every incident path in it is reached by matching a SYMPTOM, so a
// page missing from the index is unreachable during the incident it was written
// for, and an index row pointing at a deleted page sends an operator to a 404
// mid-incident. Both are compared against the directory rather than maintained by
// hand.
//
// Category: unit (TESTS.md §1) — the markdown files under docs/runbooks are the
// fixtures.

const (
	runbookDir   = "../../docs/runbooks"
	runbookIndex = "../../docs/runbooks/README.md"
)

// runbookLink matches a Markdown link whose target is a sibling runbook page.
var runbookLink = regexp.MustCompile(`\]\(([a-z0-9-]+\.md)[^)]*\)`)

func runbookPages(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(runbookDir)
	require.NoError(t, err, "the runbook directory must exist")

	var pages []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || name == "README.md" {
			continue
		}
		pages = append(pages, name)
	}
	require.NotEmpty(t, pages, "no runbook pages found under %s", runbookDir)
	sort.Strings(pages)
	return pages
}

func indexedRunbooks(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(runbookIndex)
	require.NoError(t, err, "the runbook index must exist")

	out := map[string]bool{}
	for _, match := range runbookLink.FindAllStringSubmatch(string(body), -1) {
		out[match[1]] = true
	}
	require.NotEmpty(t, out, "no runbook links parsed from %s — the index shape changed", runbookIndex)
	return out
}

func TestRunbookIndex_ListsEveryRunbook(t *testing.T) {
	indexed := indexedRunbooks(t)

	var missing []string
	for _, page := range runbookPages(t) {
		if !indexed[page] {
			missing = append(missing, page)
		}
	}
	require.Emptyf(t, missing,
		"runbooks that %s does not link, so nothing leads an operator to them: %s",
		runbookIndex, strings.Join(missing, ", "))
}

func TestRunbookIndex_LinksNoMissingRunbook(t *testing.T) {
	var broken []string
	for page := range indexedRunbooks(t) {
		if _, err := os.Stat(filepath.Join(runbookDir, page)); err != nil {
			broken = append(broken, page)
		}
	}
	sort.Strings(broken)
	require.Emptyf(t, broken,
		"%s links runbook pages that do not exist: %s",
		runbookIndex, strings.Join(broken, ", "))
}

// TestRunbookIndex_StuckSettlementIsReachableBySymptom pins the incident path a
// stalled MQTT settlement produces. The signals it names are gauges an operator
// meets on a dashboard with no error code and no failing route, so the runbook is
// only reachable if the index names the same signals the dashboard shows.
func TestRunbookIndex_StuckSettlementIsReachableBySymptom(t *testing.T) {
	const page = "stuck-mqtt-settlement.md"

	body, err := os.ReadFile(runbookIndex)
	require.NoError(t, err)
	index := string(body)
	require.Containsf(t, index, "]("+page+")",
		"%s does not link the stuck-settlement runbook", runbookIndex)

	row := ""
	for _, line := range strings.Split(index, "\n") {
		if strings.Contains(line, "]("+page+")") {
			row = line
			break
		}
	}
	for _, signal := range []string{
		"MQTTOldestUnsettledAge",
		"MQTTReceiveWindowUtilization",
	} {
		require.Containsf(t, row, signal,
			"the index row for %s must name %s — it is what an operator sees before they know what is wrong",
			page, signal)
	}

	runbook, err := os.ReadFile(filepath.Join(runbookDir, page))
	require.NoErrorf(t, err, "the stuck-settlement runbook must exist")
	content := string(runbook)
	for _, required := range []string{
		"## Symptom",
		"## Diagnosis",
		"## Action",
		"MQTTUnsettled",
		"MQTTOldestUnsettledAge",
		"MQTTReceiveWindowUtilization",
		"MQTTSessionRecoveryRecycle",
		"MQTTReceiverEmitRejected",
		"/api/v1/monitor/deephealth",
		"oldest_unsettled_age_ms",
	} {
		require.Containsf(t, content, required,
			"%s must carry %q — an operator cannot separate a stalled downstream from a wedged "+
				"route or a pinned receive window without it", page, required)
	}
}
