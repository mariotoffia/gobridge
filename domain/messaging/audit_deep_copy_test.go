package messaging_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ═══════════════════════════════════════════════════════════════════
// Envelope Deep Copy Audit Tests
//
// Validates deepCopyValue completeness for typed map/slice types
// identified by SEC-007, GO-011, QA-003:
//   - map[string]string not handled → shared backing map
//   - []int not handled → shared backing array
//   - []float64 not handled → shared backing array
// ═══════════════════════════════════════════════════════════════════

// TestEnvelope_Clone_MapStringString validates that map[string]string
// header values are deep-copied so the clone and original do not
// share the backing map.
//
// ═══════════════════════════════════════════════════════════════════
// Bug: deepCopyValue only handles map[string]any, not map[string]string.
// map[string]string falls through to the default case, returning the
// original reference.
//
// original.Headers["tags"] = map[string]string{"env": "prod"}
// clone := original.Clone()
// clone.Headers["tags"].(map[string]string)["env"] = "staging"
//
//	→ original.Headers["tags"]["env"] == "staging"  (WRONG)
//
// ═══════════════════════════════════════════════════════════════════
func TestEnvelope_Clone_MapStringString(t *testing.T) {
	original := &messaging.Envelope{
		ID: "map-str-str",
		Headers: map[string]any{
			"tags": map[string]string{"env": "prod", "region": "us-east-1"},
		},
	}

	clone := original.Clone()

	cloneTags := clone.Headers["tags"].(map[string]string)
	cloneTags["env"] = "staging"

	origTags := original.Headers["tags"].(map[string]string)
	if origTags["env"] == "staging" {
		t.Fatal("map[string]string header was not deep-copied: mutation leaked to original")
	}
	if origTags["env"] != "prod" {
		t.Fatalf("expected original env=prod, got %q", origTags["env"])
	}
}

// TestEnvelope_Clone_IntSlice validates that []int header values are
// deep-copied so the clone and original do not share the backing array.
func TestEnvelope_Clone_IntSlice(t *testing.T) {
	original := &messaging.Envelope{
		ID: "int-slice",
		Headers: map[string]any{
			"priorities": []int{1, 2, 3},
		},
	}

	clone := original.Clone()

	clonePri := clone.Headers["priorities"].([]int)
	clonePri[0] = 99

	origPri := original.Headers["priorities"].([]int)
	if origPri[0] == 99 {
		t.Fatal("[]int header was not deep-copied: mutation leaked to original")
	}
}

// TestEnvelope_Clone_Float64Slice validates that []float64 header
// values are deep-copied.
func TestEnvelope_Clone_Float64Slice(t *testing.T) {
	original := &messaging.Envelope{
		ID: "float-slice",
		Headers: map[string]any{
			"weights": []float64{0.5, 1.5, 2.5},
		},
	}

	clone := original.Clone()

	cloneW := clone.Headers["weights"].([]float64)
	cloneW[0] = 99.9

	origW := original.Headers["weights"].([]float64)
	if origW[0] == 99.9 {
		t.Fatal("[]float64 header was not deep-copied: mutation leaked to original")
	}
}
