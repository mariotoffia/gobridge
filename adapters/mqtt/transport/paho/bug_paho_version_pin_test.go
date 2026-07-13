package paho

import (
	"os"
	"strings"
	"testing"
)

// expectedPahoVersion is the paho.golang module version the MapError
// broker/transport substring table (errors.go) was verified against. It is the
// single place to update when the dependency is intentionally bumped.
const expectedPahoVersion = "v0.23.0"

// ═══════════════════════════════════════════════════════════════════════════
// B-1 (MED): MapError classifies broker/transport errors by case-insensitive
// SUBSTRING match on the paho.golang SDK's error strings (errors.go). That is a
// fragile SDK boundary: a version bump that rewords an error silently
// reclassifies it (e.g. a transient "server unavailable" falling through to the
// generic fallback, or an auth error mis-mapped), changing
// retry/backoff/credential-refresh behaviour. The substring BEHAVIOUR is pinned
// by the other error tests (bug_errors_case_test.go, analysis_errors_headers_
// test.go, errors_test.go); this test is the missing tripwire on the DEPENDENCY
// itself — it fails the build the moment go.mod's paho.golang version diverges
// from the version those substrings were verified against, so an upgrade cannot
// land without a human re-checking the SDK's error strings (F-10).
//
// On an intentional bump: re-verify the substrings in errors.go against the new
// SDK's error strings, then update expectedPahoVersion above.
//
// Mutation killed:
//   - bump the go.mod require (or the constant) without re-verifying → the
//     versions diverge and this test fails with the re-verification instruction.
//
// ═══════════════════════════════════════════════════════════════════════════
func TestBug_PahoVersion_Pinned_ForErrorTable(t *testing.T) {
	const module = "github.com/eclipse/paho.golang"

	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	got := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		// Ignore replace/exclude directives so only the require version is read.
		if strings.HasPrefix(trimmed, "replace") || strings.HasPrefix(trimmed, "exclude") {
			continue
		}
		fields := strings.Fields(trimmed)
		for i := 0; i+1 < len(fields); i++ {
			// Exact module-path match (not github.com/eclipse/paho.golang/sub),
			// with a version-shaped successor field.
			if fields[i] == module && strings.HasPrefix(fields[i+1], "v") {
				got = fields[i+1]
			}
		}
	}

	if got == "" {
		t.Fatalf("%s require not found in go.mod", module)
	}
	if got != expectedPahoVersion {
		t.Fatalf("paho.golang is %s but the MapError substring table (errors.go) was "+
			"verified against %s: re-verify the broker/transport error substrings in "+
			"errors.go against the new SDK's error strings, then update "+
			"expectedPahoVersion (B-1 / F-10)", got, expectedPahoVersion)
	}
}
