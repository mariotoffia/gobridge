package messaging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestHeaderClassificationExhaustive is the source-of-truth guard for the
// "every reserved header is classified into exactly one bucket" contract
// stated in headers.go and doc.go. It parses the const block in
// headers.go, discovers EVERY Header* string constant (the actual list a
// maintainer edits when adding a header), and asserts each one is
// classified by exactly ONE of IsInternalOnlyHeader / IsBridgeToBridgeHeader.
//
// Why parse the source instead of a hand-kept slice: the existing
// TestHeaderClassification iterates a literal []string that a contributor
// must remember to extend. Adding `HeaderXyz = "x-bridge.xyz"` and
// forgetting to classify it leaves BOTH predicates false AND both literal
// slices unchanged, so nothing fails — the exhaustiveness contract silently
// regresses. Discovering the constants from the AST removes that blind
// spot: a new unclassified constant fails here automatically.
//
// If you add a Header* constant that is intentionally NOT a classified
// reserved header, add it to the exemption set below with a reason.
func TestHeaderClassificationExhaustive(t *testing.T) {
	// HeaderPrefix is the prefix sentinel, not a classifiable header value.
	exempt := map[string]struct{}{
		"HeaderPrefix": {},
	}

	root := repoRoot(t)
	path := filepath.Join(root, "domain", "messaging", "headers.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse headers.go: %v", err)
	}

	found := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := vs.Names[0].Name
			if len(name) < len("Header") || name[:len("Header")] != "Header" {
				continue
			}
			if _, skip := exempt[name]; skip {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s value %s: %v", name, lit.Value, err)
			}
			found++

			internal := messaging.IsInternalOnlyHeader(val)
			bridge := messaging.IsBridgeToBridgeHeader(val)
			if internal == bridge {
				// Either both false (unclassified — the regression we guard
				// against) or both true (overlap — predicates not disjoint).
				t.Errorf("%s (%q): classified internal-only=%v bridge-to-bridge=%v; "+
					"want exactly one true — classify it in IsInternalOnlyHeader "+
					"or IsBridgeToBridgeHeader in headers.go",
					name, val, internal, bridge)
			}
		}
	}

	if found == 0 {
		t.Fatal("discovered zero Header* constants in headers.go; AST walk is broken")
	}
}
