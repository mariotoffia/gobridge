package ports_test

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ARCHITECTURE.md is where an implementer of a store plugin reads the port they
// are targeting, and where an operator reads what a clustered deployment can and
// cannot change live. A Go listing that names a method the port does not have
// sends an implementer at a contract the runtime never calls; a cluster section
// that denies a rollout mode the composition root wires sends an operator to a
// whole-cohort restart for a change the barrier would have rolled for them.
//
// The method set is derived from ports.DLQStore rather than restated, so the
// listing cannot drift from the interface without a red test.

const architectureDoc = "../ARCHITECTURE.md"

// architectureSection returns the body of the Markdown section that starts with
// the given heading line and ends at the next heading of any level.
func architectureSection(t *testing.T, heading string) string {
	t.Helper()
	body, err := os.ReadFile(architectureDoc)
	require.NoError(t, err, "ARCHITECTURE.md must exist")

	var section []string
	inSection := false
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, heading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(line, "#") {
			break
		}
		section = append(section, line)
	}
	require.NotEmpty(t, section, "no %q section found in %s — the heading moved", heading, architectureDoc)
	return strings.Join(section, "\n")
}

// goFenceMethods returns the method names declared inside the first ```go fence
// of a section, e.g. "    Write(ctx context.Context, ...) error" → "Write".
func goFenceMethods(t *testing.T, section string) []string {
	t.Helper()
	methodLine := regexp.MustCompile(`^\s+([A-Z][A-Za-z0-9]*)\(`)
	var methods []string
	inFence := false
	for line := range strings.SplitSeq(section, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```go"):
			inFence = true
			continue
		case strings.HasPrefix(trimmed, "```"):
			if inFence {
				sort.Strings(methods)
				return methods
			}
		}
		if !inFence {
			continue
		}
		if match := methodLine.FindStringSubmatch(line); match != nil {
			methods = append(methods, match[1])
		}
	}
	require.NotEmpty(t, methods, "no ```go listing with methods found in the section")
	return methods
}

func dlqStorePortMethods() []string {
	typ := reflect.TypeFor[ports.DLQStore]()
	methods := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		methods = append(methods, typ.Method(i).Name)
	}
	sort.Strings(methods)
	return methods
}

func TestDLQStorePublicContract_ListsThePortMethods(t *testing.T) {
	section := architectureSection(t, "### DLQStore")
	require.Equal(t, dlqStorePortMethods(), goFenceMethods(t, section),
		"the DLQStore listing in %s must name exactly the methods ports.DLQStore declares", architectureDoc)
}

// The runtime redrives a dead-lettered message by injecting a fresh envelope
// FIRST and deleting the entry only after the inject is confirmed, so a crash in
// between duplicates rather than loses. The section must say so, because an
// implementer reading "idempotent Write" plus a Replay method would build a
// store-side replay the runtime never calls.
func TestDLQStorePublicContract_DescribesInjectThenDeleteRedrive(t *testing.T) {
	section := architectureSection(t, "### DLQStore")
	require.Contains(t, section, "at-least-once",
		"the DLQStore section must state the redrive delivery guarantee")
	require.NotContains(t, section, "Replay",
		"the port has no Replay method; redrive is an admin-API sequence over Get, inject and Delete")
}

// A clustered deployment is no longer limited to whole-cohort replacement: the
// `independent` and `coordinated` rollout modes exist, and the shipped AWS
// profile wires the coordinated store in its static member-slot shape. The
// architecture page must describe that ladder rather than the ADR 0012 world it
// superseded.
func TestClusterPublicContract_ArchitectureNamesLiveRolloutModes(t *testing.T) {
	section := architectureSection(t, "### Cluster Reconfiguration")
	for _, mode := range []string{"`refuse`", "`independent`", "`coordinated`"} {
		require.Contains(t, section, mode, "the cluster section must name the %s rollout mode", mode)
	}
	require.Contains(t, section, "ADR 0013", "the cluster section must point at the coordinated rollout decision")
	require.NotContains(t, section, "there is no cluster-wide config-version",
		"the cluster section still denies the barrier ADR 0013 introduced")
}
