package messaging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// productionMustEnvelopeWhitelist enumerates the only files allowed
// to reference messaging.MustEnvelope or MustEnvelopeWithReserved
// from non-_test.go source. The H-3 rule: production code must use
// messaging.NewEnvelope and surface the error so caller-supplied
// inputs can be classified into BridgeError categories rather than
// silently substituted with a literal placeholder ID.
//
// The two storetest helpers below import "testing" and act as
// conformance suites for adapter implementations; they are test
// helpers despite not having the _test.go suffix and are explicitly
// allowed.
var productionMustEnvelopeWhitelist = map[string]struct{}{
	"ports/storetest/dlq.go":    {},
	"ports/storetest/outbox.go": {},
}

// repoRoot returns the absolute path to the gobridge repo root,
// walking up from the test file until a recognisable repo-root
// sentinel is found. go.work is gitignored and absent on CI, so we
// look for a tracked root-only file instead. The walk keeps the test
// independent of CI working-directory quirks.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Sentinels are tracked, root-only files; checking several keeps
	// the test resilient to a single file being renamed.
	sentinels := []string{"LANGUAGE.md", "ARCHITECTURE.md", "AGENTS.md"}
	dir := wd
	for i := 0; i < 12; i++ {
		for _, s := range sentinels {
			if _, err := os.Stat(filepath.Join(dir, s)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root walking up from %s", wd)
	return ""
}

// TestNoMustEnvelopeInProductionCode walks every .go source file in
// the repository and fails if MustEnvelope or MustEnvelopeWithReserved
// is referenced from a non-_test.go file that is not whitelisted as
// a test conformance suite. This is the static guardrail for the H-3
// regression.
func TestNoMustEnvelopeInProductionCode(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// envelope.go *defines* MustEnvelope — exempt the definition
		// site from the prohibition.
		if rel == "domain/messaging/envelope.go" {
			return nil
		}
		if _, ok := productionMustEnvelopeWhitelist[rel]; ok {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// Files that fail to parse are not the concern of this guardrail.
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if name != "MustEnvelope" && name != "MustEnvelopeWithReserved" {
				return true
			}
			pos := fset.Position(sel.Pos())
			violations = append(violations, rel+":"+itoa(pos.Line)+" -> "+name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("MustEnvelope used in non-test, non-whitelisted production code:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestNewEnvelopeReturnsError pins the public API contract: the
// production-safe constructor must return (*Envelope, error). A
// regression that swaps it back to a panic-on-empty signature will
// fail to compile here.
func TestNewEnvelopeReturnsError(t *testing.T) {
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "id",
		Subject: "s",
	}, time.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env == nil || env.ID != "id" {
		t.Fatalf("unexpected env: %+v", env)
	}

	// Empty ID MUST surface as an error, not a panic and not a
	// literal substitution.
	_, err = messaging.NewEnvelope(messaging.EnvelopeInput{Subject: "s"}, time.Now())
	if err == nil {
		t.Fatal("NewEnvelope with empty ID should error, got nil")
	}
}

// TestMustEnvelopeProducesUniqueFallbackIDs guards the H-3 regression
// at the helper level: even MustEnvelope must NOT collapse empty IDs
// onto a single literal. The compromise (counter-based fallback)
// keeps each invocation distinct so test fixtures can never mask
// production bugs by accidentally reusing the same envelope ID.
func TestMustEnvelopeProducesUniqueFallbackIDs(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
		if env.ID == "" || env.ID == "test-envelope" {
			t.Fatalf("regression: env.ID = %q", env.ID)
		}
		if _, dup := seen[env.ID]; dup {
			t.Fatalf("duplicate fallback ID at iter %d: %q", i, env.ID)
		}
		seen[env.ID] = struct{}{}
	}
}

// TestProductionSitesPreviouslyUsingMustEnvelope is a hard-coded
// snapshot of the five production sites that triggered the H-3
// review. Each MUST be free of MustEnvelope references; any future
// re-introduction will trip this assertion before the broader walker
// has a chance to fire.
func TestProductionSitesPreviouslyUsingMustEnvelope(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"httpapi/admin.go",
		"adapters/aws/transport/sqs/acl_inbound.go",
		"adapters/azure/transport/servicebus/acl_inbound.go",
		"adapters/amqp/transport/amqp091/acl_inbound.go",
		"adapters/amqp/transport/amqp10/acl_inbound.go",
	} {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if strings.Contains(string(data), "MustEnvelope") {
			t.Errorf("%s still references MustEnvelope; must use NewEnvelope", rel)
		}
	}
}

// itoa avoids strconv just to keep imports in the test file minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
