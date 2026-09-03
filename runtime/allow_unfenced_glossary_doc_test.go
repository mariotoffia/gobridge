package runtime_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// AllowUnfenced is consulted in exactly one place: the start-time validator,
// where it waives the rule that a direct_hold route over a shared-consumer
// source needs shared_outbox fencing (Validator.validateRoute). No store ever
// receives a token because of it and no write path reads it. The glossary is
// append-only, so the correction is a NEW row; the latest AllowUnfenced row is
// the one an engineer reads as current, and it must describe the validation
// escape rather than "permitting writes".

const glossaryDoc = "../UBIQUITOUS.md"

func TestAllowUnfencedGlossary_LatestRowIsValidationOnly(t *testing.T) {
	body, err := os.ReadFile(glossaryDoc)
	require.NoError(t, err, "the glossary must exist")

	var latest string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, "| **AllowUnfenced**") {
			latest = line
		}
	}
	require.NotEmpty(t, latest, "no AllowUnfenced row in %s", glossaryDoc)
	require.Contains(t, latest, "validation escape",
		"the current AllowUnfenced row must describe the flag as a validation escape")
	require.Contains(t, latest, "direct_hold",
		"the current AllowUnfenced row must name the delivery mode the escape applies to")
	require.NotContains(t, latest, "permitting writes",
		"the current AllowUnfenced row still describes a fencing model the runtime does not have")
}
