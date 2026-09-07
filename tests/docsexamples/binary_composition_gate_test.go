package docsexamples_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBinaryComposition_IntegrationGateIncludesAllFamilies verifies the
// integration gate runs tagged tests without -short and propagates failures.
// Without the tagged pass, family integration tests are never compiled.
// Category: unit (TESTS.md section 1).
func TestBinaryComposition_IntegrationGateIncludesAllFamilies(t *testing.T) {
	require.Contains(t, makefileRecipe(t, "test-integration"),
		"go -C cmd/gobridge test -tags gobridge_all -count=1 -p 1 -race -timeout 600s -v ./... || rc=$$?")
}
