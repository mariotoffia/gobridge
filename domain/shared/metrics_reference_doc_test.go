package shared_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The metric catalogue an operator reads is the only place a wire name is
// published. A metric the page omits cannot be alarmed on or charted, because
// nothing else names it; a metric the page invents sends an operator looking for
// a series that is never emitted. Both failures are silent in production and
// neither shows up in any other test, so the two sets are compared directly:
// the wire values declared in metrics.go against the rows of the sections of the
// catalogue this package owns.
//
// Adapter-owned sections (the store, transport and exporter tables) name metrics
// declared in other modules and are excluded BY HEADING rather than by guessing,
// so a new section added to the page fails here until it is classified.

const metricsReferenceDoc = "../../docs/aws-deployment/monitoring.md"

const metricsReferenceHeading = "### Key Metrics"

// sharedOwnedGroups are the bold sub-headings of the catalogue whose rows must
// be exactly the metric constants declared in this package.
var sharedOwnedGroups = map[string]bool{
	"Messages & delivery": true,
	"Outbox":              true,
	"Lease":               true,
	"DLQ":                 true,
	"Circuit breaker":     true,
	"Processor":           true,
	"Session & route":     true,
	"Credentials":         true,
	"Reconfiguration":     true,
	"Cluster rollout":     true,
	// The two transport-agnostic wrapper metrics. Declared here, emitted only by
	// the opt-in instrumented-receiver wrappers.
	"Generic delivery (opt-in wrappers)": true,
}

// adapterOwnedGroups name metrics declared in adapter modules, which this
// package cannot see. They are listed so an UNCLASSIFIED group is an error
// rather than a silent skip.
var adapterOwnedGroups = map[string]bool{
	"Store health":          true,
	"Transport (SQS)":       true,
	"Transport (MQTT)":      true,
	"Exporter self-metrics": true,
}

var (
	docGroupHeading = regexp.MustCompile(`^\*\*(.+?)\*\*\s*$`)
	docMetricRow    = regexp.MustCompile("^\\|\\s*`([A-Za-z0-9]+)`\\s*\\|")
)

// declaredMetricNames returns the wire values of every Metric* constant declared
// in metrics.go, keyed by the constant identifier.
func declaredMetricNames(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "metrics.go", nil, 0)
	require.NoError(t, err, "metrics.go must parse")

	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				// MetricNamespace is the namespace every metric publishes under,
				// not a metric of its own.
				if !strings.HasPrefix(name.Name, "Metric") || name.Name == "MetricNamespace" {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				wire, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				out[name.Name] = wire
			}
		}
	}
	require.NotEmpty(t, out, "no Metric* constants parsed from metrics.go")
	return out
}

// documentedMetricGroups returns the metric names of the catalogue, keyed by the
// bold group heading they sit under.
func documentedMetricGroups(t *testing.T) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(metricsReferenceDoc)
	require.NoError(t, err, "the metric catalogue page must exist")

	groups := map[string][]string{}
	inSection, inFence, group := false, false, ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, metricsReferenceHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		// A fenced example may open with a line the scanners below would read as
		// a heading or a table row; skipping fences keeps the scan on the tables.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "#") {
			break // the next heading ends the catalogue
		}
		if match := docGroupHeading.FindStringSubmatch(line); match != nil {
			group = match[1]
			if _, seen := groups[group]; !seen {
				groups[group] = nil
			}
			continue
		}
		if match := docMetricRow.FindStringSubmatch(line); match != nil {
			require.NotEmpty(t, group, "metric row %q sits under no bold group heading", match[1])
			groups[group] = append(groups[group], match[1])
		}
	}
	require.NotEmpty(t, groups, "no metric groups parsed from %s — the page structure changed", metricsReferenceDoc)
	return groups
}

func TestMetricsReference_DocumentsEveryDeclaredMetric(t *testing.T) {
	declared := declaredMetricNames(t)
	groups := documentedMetricGroups(t)

	documented := map[string]bool{}
	for group, names := range groups {
		require.Truef(t, sharedOwnedGroups[group] || adapterOwnedGroups[group],
			"metric group %q in %s is classified neither as owned by this package nor by an adapter; "+
				"add it to one of the two lists so its rows are checked or knowingly skipped",
			group, metricsReferenceDoc)
		if !sharedOwnedGroups[group] {
			continue
		}
		for _, name := range names {
			require.Falsef(t, documented[name], "metric %q is documented twice", name)
			documented[name] = true
		}
	}

	var missing []string
	for constant, wire := range declared {
		if !documented[wire] {
			missing = append(missing, wire+" ("+constant+")")
		}
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"metrics declared in metrics.go with no row in %s: %s",
		metricsReferenceDoc, strings.Join(missing, ", "))
}

func TestMetricsReference_DocumentsNoMetricThatIsNotEmitted(t *testing.T) {
	declared := declaredMetricNames(t)
	emitted := map[string]bool{}
	for _, wire := range declared {
		emitted[wire] = true
	}

	var invented []string
	for group, names := range documentedMetricGroups(t) {
		if !sharedOwnedGroups[group] {
			continue
		}
		for _, name := range names {
			if !emitted[name] {
				invented = append(invented, name+" (under **"+group+"**)")
			}
		}
	}
	sort.Strings(invented)
	require.Emptyf(t, invented,
		"%s documents metrics no constant in metrics.go declares: %s",
		metricsReferenceDoc, strings.Join(invented, ", "))
}

func TestMetricsReference_EveryOwnedGroupIsPresent(t *testing.T) {
	groups := documentedMetricGroups(t)
	var absent []string
	for group := range sharedOwnedGroups {
		if _, ok := groups[group]; !ok {
			absent = append(absent, group)
		}
	}
	sort.Strings(absent)
	require.Emptyf(t, absent,
		"%s no longer carries the metric groups this package owns: %s — a removed group would "+
			"silently stop checking every metric in it",
		metricsReferenceDoc, strings.Join(absent, ", "))
}
