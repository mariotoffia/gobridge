package validation_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// Test_TierB_Validation_YAMLUnparseable covers matrix row 1
// ("yaml unparseable").
//
// Matrix wording: "bridge.yaml: <yaml lib error with line/col>".
//
// source.Materialize wraps every config.ParseFile failure in
// source.ErrYamlParse, whose message IS the required "bridge.yaml: "
// prefix, and keeps the yaml-lib error wrapped behind it so the
// line/col detail survives for the operator.
func Test_TierB_Validation_YAMLUnparseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	// Unbalanced flow mapping on line 3: the yaml decoder reports a
	// position, which must reach the operator.
	broken := "bridge:\n  id: demo\n  cluster: {unclosed\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := source.NewAsset(path).Materialize()
	if err == nil {
		t.Fatal("Materialize on unparseable yaml must fail")
	}
	if !errors.Is(err, source.ErrYamlParse) {
		t.Fatalf("error must wrap source.ErrYamlParse, got %v", err)
	}
	if !strings.Contains(err.Error(), "bridge.yaml: ") {
		t.Fatalf("matrix requires the %q prefix, got %v", "bridge.yaml: ", err)
	}
	// The yaml lib's line/col detail must not be swallowed by the wrap.
	if !strings.Contains(err.Error(), "line ") {
		t.Fatalf("matrix requires yaml line/col detail to survive the wrap, got %v", err)
	}
}

// Test_TierB_Validation_Stage1Validator covers matrix row 2
// ("Stage-1 validator fail").
//
// Matrix wording: "bridge.yaml: <stage-1 error>".
//
// Same boundary as row 1: the document parses as yaml but fails the
// stage-1 strict decode (unknown key). Materialize applies the same
// source.ErrYamlParse wrap, so the prefix appears and the stage-1
// field detail stays readable behind it.
func Test_TierB_Validation_Stage1Validator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	// Valid yaml, invalid blueprint: "shutdown_timout" is a typo the
	// stage-1 strict decode must reject rather than silently discard.
	typo := "bridge:\n  id: demo\n  shutdown_timout: 5s\n"
	if err := os.WriteFile(path, []byte(typo), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := source.NewAsset(path).Materialize()
	if err == nil {
		t.Fatal("Materialize on a stage-1 validation failure must fail")
	}
	if !errors.Is(err, source.ErrYamlParse) {
		t.Fatalf("error must wrap source.ErrYamlParse, got %v", err)
	}
	if !strings.Contains(err.Error(), "bridge.yaml: ") {
		t.Fatalf("matrix requires the %q prefix, got %v", "bridge.yaml: ", err)
	}
	// The offending field must be named so the operator can find it.
	if !strings.Contains(err.Error(), "shutdown_timout") {
		t.Fatalf("matrix requires the stage-1 field detail to survive the wrap, got %v", err)
	}
}

// Matrix row 10 ("Multiple GoBridge in same stack") is covered by
// Test_TierB_Validation_MultipleGoBridgeInStack in
// constructs/internal/singleton/matrix_test.go — kept there because
// triggering the panic requires the GoBridgeSingle facade and the
// CDK harness already wired into that package.
