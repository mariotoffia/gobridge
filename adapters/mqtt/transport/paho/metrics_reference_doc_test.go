package paho_test

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

// Every metric this adapter emits is a signal an operator is expected to act on,
// and the diagnostic-metric table is where its meaning is published. A metric
// missing from that table is one an operator meets first in an alarm, with
// nothing to read; a name in the table that no constant declares sends them
// looking for a series nothing emits. Neither shows up anywhere else, so the
// declared wire values and the documented rows are compared directly.

const mqttMetricsReferenceDoc = "../../../../docs/adapter-diagnostic-metrics.md"

const mqttMetricsReferenceHeading = "### MQTT (`adapters/mqtt/transport/paho`)"

// docMetricCell matches every backticked name in the first cell of a Markdown
// table row. A cell may name more than one metric when their meaning is shared.
var docMetricCell = regexp.MustCompile(`^\|([^|]*)\|`)

var docBacktickedName = regexp.MustCompile("`([A-Za-z0-9]+)`")

// declaredMQTTMetricNames returns the wire values of every Metric* constant
// declared by this adapter.
func declaredMQTTMetricNames(t *testing.T) map[string]string {
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
				if !strings.HasPrefix(name.Name, "Metric") {
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

// documentedMQTTMetrics returns the metric names named in the first column of
// the adapter's diagnostic-metric table.
func documentedMQTTMetrics(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(mqttMetricsReferenceDoc)
	require.NoError(t, err, "the troubleshooting page must exist")

	out := map[string]bool{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, mqttMetricsReferenceHeading) {
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
		if strings.HasPrefix(line, "#") {
			break // the next adapter's section ends this one
		}
		cell := docMetricCell.FindStringSubmatch(line)
		if cell == nil {
			continue
		}
		for _, name := range docBacktickedName.FindAllStringSubmatch(cell[1], -1) {
			out[name[1]] = true
		}
	}
	require.NotEmpty(t, out,
		"no metric rows parsed from the %s section of %s — the table shape changed",
		mqttMetricsReferenceHeading, mqttMetricsReferenceDoc)
	return out
}

func TestMQTTMetricsReference_DocumentsEveryDeclaredMetric(t *testing.T) {
	documented := documentedMQTTMetrics(t)

	var missing []string
	for constant, wire := range declaredMQTTMetricNames(t) {
		if !documented[wire] {
			missing = append(missing, wire+" ("+constant+")")
		}
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"metrics this adapter emits with no row in %s: %s",
		mqttMetricsReferenceDoc, strings.Join(missing, ", "))
}

func TestMQTTMetricsReference_DocumentsNoMetricThatIsNotEmitted(t *testing.T) {
	emitted := map[string]bool{}
	for _, wire := range declaredMQTTMetricNames(t) {
		emitted[wire] = true
	}

	var invented []string
	for name := range documentedMQTTMetrics(t) {
		if !emitted[name] {
			invented = append(invented, name)
		}
	}
	sort.Strings(invented)
	require.Emptyf(t, invented,
		"%s documents MQTT metrics no constant in metrics.go declares: %s",
		mqttMetricsReferenceDoc, strings.Join(invented, ", "))
}
