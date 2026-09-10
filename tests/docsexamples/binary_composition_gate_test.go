package docsexamples_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIntegrationGate_PreparesLocalWorkspace verifies fresh checkouts use local sibling modules.
func TestIntegrationGate_PreparesLocalWorkspace(t *testing.T) {
	recipe := strings.TrimSpace(makefileRecipe(t, "test-integration"))
	require.True(t, strings.HasPrefix(recipe, "@test -f go.work || $(MAKE) dev\n"),
		"integration tests must prepare the workspace before resolving sibling modules")
}

// TestBinaryComposition_IntegrationGateIncludesAllFamilies verifies the
// integration gate runs tagged tests without -short and propagates failures.
// Without the tagged pass, family integration tests are never compiled.
// Category: unit (TESTS.md section 1).
func TestBinaryComposition_IntegrationGateIncludesAllFamilies(t *testing.T) {
	require.Contains(t, makefileRecipe(t, "test-integration"),
		"go -C cmd/gobridge test -tags gobridge_all -count=1 -p 1 -race -timeout 600s -v ./... || rc=$$?")
}
