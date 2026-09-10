package cloudwatch_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cwmetrics "github.com/mariotoffia/gobridge/adapters/aws/metrics/cloudwatch"
)

// The built-in alarms all read DIMENSIONLESS series, and the runtime emits most
// metrics with a route, session or partition dimension. The rollup list is what
// bridges the two: a metric that is alarmed on but not rolled up has nothing the
// alarm can ever match, and the alarm sits at INSUFFICIENT_DATA — silent, not
// failed, and indistinguishable from health.
//
// The deployment page is where an operator learns which metrics to configure the
// exporter with, and the CDK bundle's own suite checks its alarms against that
// same table. Comparing this list against the page therefore closes the loop
// across two modules that cannot import each other.

const rollupReferenceDoc = "../../../../docs/aws-deployment/alarms.md"

const rollupReferenceHeading = "## Rollup metrics the built-in alarms require"

var rollupDocRow = regexp.MustCompile("^\\|\\s*`([A-Za-z0-9]+)`\\s*\\|")

func documentedRollupMetrics(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(rollupReferenceDoc)
	require.NoError(t, err, "the alarm page must exist")

	out := map[string]bool{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, rollupReferenceHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		if match := rollupDocRow.FindStringSubmatch(line); match != nil {
			out[match[1]] = true
		}
	}
	require.NotEmptyf(t, out, "no rollup metrics parsed from the %q section of %s — the table shape changed",
		rollupReferenceHeading, rollupReferenceDoc)
	return out
}

func TestDefaultRollupMetrics_MatchesThePublishedList(t *testing.T) {
	documented := documentedRollupMetrics(t)

	shipped := map[string]bool{}
	for _, name := range cwmetrics.DefaultRollupMetrics() {
		require.Falsef(t, shipped[name], "DefaultRollupMetrics returns %q twice", name)
		shipped[name] = true
	}

	var undocumented, unshipped []string
	for name := range shipped {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	for name := range documented {
		if !shipped[name] {
			unshipped = append(unshipped, name)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unshipped)

	require.Emptyf(t, undocumented,
		"DefaultRollupMetrics rolls these up but %s does not list them: %s",
		rollupReferenceDoc, strings.Join(undocumented, ", "))
	require.Emptyf(t, unshipped,
		"%s tells operators these are rolled up but DefaultRollupMetrics does not emit a "+
			"dimensionless copy, so every alarm on them stays at INSUFFICIENT_DATA: %s",
		rollupReferenceDoc, strings.Join(unshipped, ", "))
}
