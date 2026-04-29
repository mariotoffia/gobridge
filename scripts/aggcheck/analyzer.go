// aggcheck implements a go/analysis pass that enforces a simple
// aggregate-root naming convention in the domain package.
//
// The convention:
//
//   - A struct type that has at least one unexported field AND at
//     least one method with a value receiver returning the type
//     (i.e., a "transition method" producing a new value) is
//     considered an aggregate.
//   - Aggregate types MUST live in a file whose name ends in
//     "_aggregate.go".
//   - Aggregate types MUST declare a method named "Validate" that
//     returns an error so the invariants are explicit, not implicit.
//
// Pure value objects (no unexported state, no transition methods)
// and types whose only "mutation" is via a pointer receiver (i.e.,
// in-place mutation rather than a transition) are exempt by
// definition — they should remain immutable values.
//
// The analyzer scans the domain package only. Tests are exempt.
package main

import (
	"go/ast"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer is the singleton aggcheck analysis pass.
var Analyzer = &analysis.Analyzer{
	Name:     "aggcheck",
	Doc:      "checks that DDD aggregate-root types in domain/ live in _aggregate.go files and declare a Validate() error method",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	if !strings.HasSuffix(pass.Pkg.Path(), "/domain") &&
		!strings.Contains(pass.Pkg.Path(), "/domain/") {
		return nil, nil //nolint:nilnil
	}

	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Index types declared in the package: name → file.
	typeFile := map[string]string{}
	// Index methods on those types: typeName → set of method names.
	methods := map[string]map[string]bool{}
	// Index unexported field counts and value-receiver transition
	// methods to identify aggregate candidates.
	hasUnexportedField := map[string]bool{}
	hasTransition := map[string]bool{}

	insp.Preorder([]ast.Node{(*ast.GenDecl)(nil), (*ast.FuncDecl)(nil)}, func(n ast.Node) {
		switch d := n.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				name := ts.Name.Name
				typeFile[name] = pass.Fset.Position(ts.Pos()).Filename
				if hasUnexported(st) {
					hasUnexportedField[name] = true
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) == 0 {
				return
			}
			recvType, isPtr := receiverTypeName(d.Recv.List[0].Type)
			if recvType == "" {
				return
			}
			if methods[recvType] == nil {
				methods[recvType] = map[string]bool{}
			}
			methods[recvType][d.Name.Name] = true
			// A "transition method" is a value-receiver method whose
			// return type is the same value type — the aggregate hand
			// produces a new value rather than mutating in place.
			if !isPtr && returnsSelf(d.Type, recvType) {
				hasTransition[recvType] = true
			}
		}
	})

	for name, file := range typeFile {
		if !hasUnexportedField[name] || !hasTransition[name] {
			continue // not an aggregate by our criteria.
		}
		base := filepath.Base(file)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		if !strings.HasSuffix(base, "_aggregate.go") {
			pass.Reportf(0,
				"aggregate-like type %q in %s should live in a file ending '_aggregate.go' so its role is visible at a glance",
				name, base)
		}
		if !methods[name]["Validate"] {
			pass.Reportf(0,
				"aggregate-like type %q (in %s) must declare a Validate() error method to make invariants explicit",
				name, base)
		}
	}

	return nil, nil //nolint:nilnil
}

// hasUnexported returns true if the struct has at least one
// unexported field.
func hasUnexported(st *ast.StructType) bool {
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			if !n.IsExported() {
				return true
			}
		}
	}
	return false
}

// receiverTypeName extracts the receiver type name and whether it
// is a pointer receiver. Returns ("", false) if the receiver shape
// is not a recognised T or *T.
func receiverTypeName(expr ast.Expr) (string, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name, false
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name, true
		}
	}
	return "", false
}

// returnsSelf returns true if the function type returns the named
// value type as its first or only return value.
func returnsSelf(ft *ast.FuncType, typeName string) bool {
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return false
	}
	first := ft.Results.List[0].Type
	id, ok := first.(*ast.Ident)
	return ok && id.Name == typeName
}
