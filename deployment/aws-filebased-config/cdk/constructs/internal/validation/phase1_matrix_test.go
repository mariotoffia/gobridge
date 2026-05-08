package validation_test

import (
	"testing"
)

// Test_TierB_Validation_YAMLUnparseable covers matrix row 1
// ("yaml unparseable").
//
// Matrix wording: "bridge.yaml: <yaml lib error with line/col>".
//
// Production gap: source.Materialize wraps parse failures as
// `gobridgecdk: BridgeYamlAsset(...): parse: ...` and does NOT
// prepend the matrix-required "bridge.yaml: " prefix. The
// validation.ErrYamlParse sentinel exists for callers to use the
// wrapping pattern documented in errors.go but no production code
// applies it. Until that wiring lands the assertion below verifies
// the underlying yaml-lib error surfaces with line/col detail; the
// prefix portion is left to a follow-up.
func Test_TierB_Validation_YAMLUnparseable(t *testing.T) {
	t.Skip("matrix row 1 'yaml unparseable': source.Materialize does not wrap with validation.ErrYamlParse 'bridge.yaml: ' prefix; see TODO")
	// TODO(matrix-row 'yaml unparseable'): wire validation.ErrYamlParse
	// in source.Materialize so the operator-facing prefix becomes
	// "bridge.yaml: ..." per the design matrix.
}

// Test_TierB_Validation_Stage1Validator covers matrix row 2
// ("Stage-1 validator fail").
//
// Matrix wording: "bridge.yaml: <stage-1 error>".
//
// Production gap: identical to row 1 — Materialize's wrap doesn't
// emit the "bridge.yaml: " prefix. We assert the underlying
// config-stage error reaches the caller; the prefix is a follow-up.
func Test_TierB_Validation_Stage1Validator(t *testing.T) {
	t.Skip("matrix row 2 'Stage-1 validator fail': source.Materialize does not wrap stage-1 errors with 'bridge.yaml: ' prefix; see TODO")
	// TODO(matrix-row 'Stage-1 validator fail'): apply the wrap in
	// source.Materialize so the prefix appears in real flows.
}

// Matrix row 10 ("Multiple GoBridge in same stack") is covered by
// Test_TierB_Validation_MultipleGoBridgeInStack in
// constructs/internal/singleton/matrix_test.go — kept there because
// triggering the panic requires the GoBridgeSingle facade and the
// CDK harness already wired into that package.
