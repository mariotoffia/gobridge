package ports_test

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The `bridge.cluster` field table is where an operator discovers that a
// coordinated rollout exists at all. Three of its four keys — rollout, members and
// confirm_window — are load-bearing for the barrier: without them the cohort
// refuses every live config change, and the validator's errors are the only other
// place they are named. A key the table omits is a feature the operator cannot
// find; a key the table invents is one they will configure and never see take
// effect.
//
// The parsed set is derived from ports.ClusterConfig rather than restated, so the
// page cannot drift from the model without a red test.

const clusterReferenceDoc = "../docs/configuration-reference.md"

const clusterReferenceHeading = "### `bridge.cluster`"

// docFieldRow matches a Markdown table row whose first cell is a backticked field
// name, e.g. "| `confirm_window` | duration | no | ... |".
var docFieldRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_]+)`\\s*\\|")

func documentedClusterFields(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(clusterReferenceDoc)
	require.NoError(t, err, "the configuration reference page must exist")

	fields := map[string]bool{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, clusterReferenceHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		// A fenced example may contain a line starting with "#" (a YAML comment, a
		// shell prompt). Ending the scan there would silently stop checking the rows
		// below it, so fences are tracked and their contents skipped.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "#") {
			break // the next heading ends the cluster section
		}
		if match := docFieldRow.FindStringSubmatch(line); match != nil {
			fields[match[1]] = true
		}
	}
	require.NotEmpty(t, fields, "no field rows parsed from the %s section of %s — the table shape changed",
		clusterReferenceHeading, clusterReferenceDoc)
	return fields
}

func parsedClusterFields(t *testing.T) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(ports.ClusterConfig{})
	fields := map[string]bool{}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
	}
	return fields
}

func TestClusterConfigReference_DocumentsEveryParsedField(t *testing.T) {
	documented := documentedClusterFields(t)
	parsed := parsedClusterFields(t)

	var undocumented, phantom []string
	for name := range parsed {
		if !documented[name] {
			undocumented = append(undocumented, name)
		}
	}
	for name := range documented {
		if !parsed[name] {
			phantom = append(phantom, name)
		}
	}
	require.Empty(t, undocumented,
		"bridge.cluster keys the parser reads but %s does not document; an operator cannot discover them",
		clusterReferenceDoc)
	require.Empty(t, phantom,
		"keys documented under %s that bridge.cluster does not read; setting them would be silently ignored",
		clusterReferenceHeading)
}
