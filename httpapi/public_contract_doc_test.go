package httpapi_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The monitor and admin servers are the only surface an orchestrator, a load
// balancer or an on-call operator ever touches, so the pages that describe them
// are load-bearing: a probe designed from a false readiness contract steers
// traffic at an instance that cannot serve it, a runbook that names a role the
// runtime never reports sends an operator hunting for a state that does not
// exist, and an endpoint table that lists a path the mux does not register is a
// 404 mid-incident.
//
// Every expectation below is derived from an exported symbol or from the OpenAPI
// document the handlers are written against — never from a source line number.
//
// Category: unit (TESTS.md §1) — the Markdown and YAML files are the fixtures.

const (
	healthShutdownDoc = "../docs/health-and-shutdown.md"
	monitorAPIDoc     = "../docs/http-api-monitor.md"
	nodeDownRunbook   = "../docs/runbooks/node-down-failover.md"
	architecturePage  = "../ARCHITECTURE.md"
	openAPIPaths      = "../spec/httpapi/http-api.yaml"
	openAPIComponents = "../spec/httpapi/components.yaml"
	releaseNotesDoc   = "../docs/release-notes.md"
	adrIndexDoc       = "../docs/adr/README.md"
)

func readDoc(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err, "%s must exist", path)
	return string(body)
}

// The bare /ready probe (no ?level=) gates on ports.LevelFull: every session
// subscribed AND every route able to dispatch, with a standby capped below it.
// A page that says the bare probe passes before connection or subscription
// describes a contract handleReady does not provide.
func TestReadyPublicContract_BareProbeRequiresFullLevel(t *testing.T) {
	want := fmt.Sprintf("requires the `%s` readiness level", ports.LevelFull.String())
	for _, page := range []string{healthShutdownDoc, monitorAPIDoc} {
		body := readDoc(t, page)
		require.Contains(t, body, want,
			"%s must state the level the bare /ready probe gates on", page)
	}
	require.NotContains(t, readDoc(t, healthShutdownDoc),
		"does **not** guarantee transport sessions are connected",
		"%s still claims the bare probe passes before sessions are connected", healthShutdownDoc)
}

// The role the probes report is the runtime's lease-ownership vocabulary and
// nothing else. A runbook telling an operator to look for `leader` sends them
// looking for a value that is never emitted.
func TestReadyPublicContract_RoleVocabularyMatchesRuntime(t *testing.T) {
	roles := []string{ports.RoleActive, ports.RoleStandby, ports.RoleStandalone}
	foreign := regexp.MustCompile("`leader`|role: leader")
	for _, page := range []string{nodeDownRunbook, monitorAPIDoc, architecturePage} {
		body := readDoc(t, page)
		for _, role := range roles {
			require.Contains(t, body, "`"+role+"`", "%s must name the %q role", page, role)
		}
		require.NotRegexp(t, foreign, body, "%s names a role the runtime never reports", page)
	}
}

// openAPIOperations returns "METHOD /path" for every operation under `paths:`.
func openAPIOperations(t *testing.T) []string {
	t.Helper()
	pathLine := regexp.MustCompile(`^  (/[^:\s]+):\s*$`)
	methodLine := regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	var ops []string
	inPaths, current := false, ""
	for line := range strings.SplitSeq(readDoc(t, openAPIPaths), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
			continue
		case inPaths && len(line) > 0 && line[0] != ' ':
			inPaths = false
		}
		if !inPaths {
			continue
		}
		if match := pathLine.FindStringSubmatch(line); match != nil {
			current = match[1]
			continue
		}
		if match := methodLine.FindStringSubmatch(line); match != nil && current != "" {
			ops = append(ops, strings.ToUpper(match[1])+" "+current)
		}
	}
	require.NotEmpty(t, ops, "no operations parsed from %s — the document shape changed", openAPIPaths)
	sort.Strings(ops)
	return ops
}

// documentedOperations returns "METHOD /path" for every row of the endpoint
// tables under the given ARCHITECTURE.md headings.
func documentedOperations(t *testing.T, headings ...string) []string {
	t.Helper()
	row := regexp.MustCompile("^\\|\\s*`([A-Z]+)`\\s*\\|\\s*`(/[^`]+)`")
	body := readDoc(t, architecturePage)
	var ops []string
	for _, heading := range headings {
		inSection, found := false, false
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(line, heading) {
				inSection, found = true, true
				continue
			}
			if !inSection {
				continue
			}
			if strings.HasPrefix(line, "#") {
				break
			}
			if match := row.FindStringSubmatch(line); match != nil {
				ops = append(ops, match[1]+" "+match[2])
			}
		}
		require.True(t, found, "no %q section in %s", heading, architecturePage)
	}
	require.NotEmpty(t, ops, "no endpoint rows parsed under %v in %s", headings, architecturePage)
	sort.Strings(ops)
	return ops
}

// The endpoint tables in ARCHITECTURE.md must list exactly the operations the
// OpenAPI document declares — the same document the handlers are registered
// from — so a renamed or added path cannot leave the architecture page pointing
// at a 404.
func TestEndpointPublicContract_ArchitectureTablesMatchTheOpenAPIPaths(t *testing.T) {
	require.Equal(t, openAPIOperations(t),
		documentedOperations(t, "### Admin Server", "### Monitor Server"),
		"ARCHITECTURE.md endpoint tables must match spec/httpapi/http-api.yaml")
}

// Redrive injects a fresh envelope first and deletes the entry only after the
// inject is confirmed: a crash in between duplicates, never loses. Every place
// that publishes the guarantee must say so — the wire contract, the release
// notes an operator reads before upgrading, and the ADR index, where the
// superseded at-most-once decision must be marked as such.
func TestRedrivePublicContract_StatedAsInjectThenDelete(t *testing.T) {
	components := readDoc(t, openAPIComponents)
	require.Contains(t, components, "at-least-once", "%s must state the redrive guarantee", openAPIComponents)
	require.NotContains(t, components, "at-most-once", "%s still describes the superseded claim-by-delete redrive", openAPIComponents)

	notes := readDoc(t, releaseNotesDoc)
	require.NotContains(t, notes, "DLQ redrive is at-most-once", "%s still announces the superseded redrive guarantee", releaseNotesDoc)
	require.Contains(t, notes, "inject", "%s must describe the inject-then-delete redrive", releaseNotesDoc)

	index := readDoc(t, adrIndexDoc)
	require.Regexp(t, regexp.MustCompile(`\[0006\].*\|\s*superseded by 00\d\d`), index,
		"%s must mark ADR 0006 (at-most-once redrive) as superseded", adrIndexDoc)
}
