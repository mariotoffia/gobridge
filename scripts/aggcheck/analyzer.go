// aggcheck implements a go/analysis pass that enforces DDD
// aggregate-root encapsulation conventions in the domain package.
//
// Two independent signals mark a struct type as an aggregate root:
//
//   - EXPLICIT MARKER (preferred): the type's doc comment carries the
//     machine marker on its own line — "// aggregate-root". The marker
//     makes the role unambiguous and, crucially, lets the analyzer
//     guard POINTER-mutating aggregates (OutboxRecord, Envelope-style
//     records, credential sets) that the value-receiver heuristic below
//     cannot see. The token is hyphenated so ordinary prose such as
//     "is the aggregate root for …" never trips the detector.
//   - LEGACY HEURISTIC (domain only): a struct that has at least one
//     unexported field AND at least one VALUE-receiver method returning
//     the type itself (a transition producing a new value).
//
// Rules for MARKED aggregate roots (checked wherever they are declared):
//
//   - No exported mutable fields. An aggregate root owns its invariants;
//     an exported field lets a caller mutate state without passing
//     through a guarded transition, so every exported field is flagged.
//     Encapsulate it behind a read-only accessor and a guarded mutator.
//   - No unguarded pointer transitions. An EXPORTED pointer-receiver
//     method that assigns to a receiver field but returns nothing is an
//     "unguarded" transition: it mutates aggregate state with no way to
//     reject an invariant violation. A guarded mutator returns a result
//     (typically an error) the caller must handle. Pointer-mutating
//     aggregates are exactly the shape the value-receiver heuristic
//     missed; the marker closes that gap.
//
// Per-method opt-out: a method whose doc comment carries
// "//aggcheck:allow-unguarded" (or "// aggcheck:allow-unguarded") on its
// own line is exempt from the unguarded-pointer-transition rule. Use this
// only for exported void pointer-receiver mutators that carry NO failable
// invariant (documented, always-valid extension points). Adding a
// meaningless error return that no caller ever checks is worse than the
// exemption.
//
// Rules for HEURISTIC value-style aggregates (domain only):
//
//   - MUST live in a file whose name ends in "_aggregate.go".
//   - MUST declare a "Validate() error" method so invariants are
//     explicit, not implicit.
//
// Pure value objects (no unexported state, no transition methods) and
// any unmarked, non-heuristic type are exempt — they should remain
// immutable values. A value object whose derivation legitimately returns
// a new copy of itself (tripping the heuristic) declares the
// "// value-object" marker to record that role and opt out of the
// heuristic track. The heuristic track scans the domain package only;
// the marker track runs on any package so an explicitly marked root is
// guarded wherever it lives (and so the rule is unit-testable). Tests
// are exempt.
package main

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// aggregateMarker is the doc-comment token that explicitly designates a
// struct type as a DDD aggregate root.
const aggregateMarker = "aggregate-root"

// allowUnguardedMarker is the per-method doc-comment token that exempts
// an exported void pointer-receiver mutator from the unguarded-pointer-
// transition rule. Use only for always-valid extension points where a
// failable invariant is absent and a mandatory error return would be noise.
const allowUnguardedMarker = "aggcheck:allow-unguarded"

// valueObjectMarker is the doc-comment token that explicitly designates a
// struct type as a pure value object, exempting it from the legacy value
// aggregate heuristic. The heuristic ("unexported field + value-receiver
// method returning the type itself") legitimately fires on value objects
// whose derivations return a new instance of themselves (e.g. a Secret
// returning a re-moded copy). Such types are immutable values, not
// aggregates; the marker records that role so the heuristic stays silent
// without weakening it for genuine value-style aggregates.
const valueObjectMarker = "value-object"

// Analyzer is the singleton aggcheck analysis pass.
var Analyzer = &analysis.Analyzer{
	Name:     "aggcheck",
	Doc:      "checks DDD aggregate-root encapsulation: marked roots forbid exported mutable fields and unguarded pointer transitions; heuristic value aggregates live in _aggregate.go and declare Validate()",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// aggType carries everything the rules need about one struct declaration.
type aggType struct {
	name        string
	file        string
	marked      bool
	valueObject bool
	st          *ast.StructType
	pos         token.Pos
}

// member names a struct field or method together with its position so
// diagnostics point at the offending line.
type member struct {
	name string
	pos  token.Pos
}

func run(pass *analysis.Pass) (interface{}, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inDomain := strings.HasSuffix(pass.Pkg.Path(), "/domain") ||
		strings.Contains(pass.Pkg.Path(), "/domain/")

	decls := map[string]*aggType{}
	methods := map[string]map[string]bool{} // typeName -> method-name set
	hasValueTransition := map[string]bool{} // legacy heuristic transition
	hasUnexportedField := map[string]bool{}
	unguarded := map[string][]member{} // typeName -> unguarded exported mutators

	insp.Preorder([]ast.Node{(*ast.GenDecl)(nil), (*ast.FuncDecl)(nil)}, func(n ast.Node) {
		switch d := n.(type) {
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				return
			}
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
				decls[name] = &aggType{
					name:        name,
					file:        pass.Fset.Position(ts.Pos()).Filename,
					marked:      hasMarker(ts.Doc, aggregateMarker) || hasMarker(d.Doc, aggregateMarker),
					valueObject: hasMarker(ts.Doc, valueObjectMarker) || hasMarker(d.Doc, valueObjectMarker),
					st:          st,
					pos:         ts.Pos(),
				}
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
			if !isPtr && returnsSelf(d.Type, recvType) {
				hasValueTransition[recvType] = true
			}
			if isPtr && d.Name.IsExported() && returnsNothing(d.Type) && mutatesReceiver(d) &&
				!hasMarker(d.Doc, allowUnguardedMarker) {
				unguarded[recvType] = append(unguarded[recvType], member{d.Name.Name, d.Name.Pos()})
			}
		}
	})

	for name, at := range decls {
		if strings.HasSuffix(filepath.Base(at.file), "_test.go") {
			continue
		}
		switch {
		case at.marked:
			checkMarkedRoot(pass, at, unguarded[name])
		case at.valueObject:
			// Explicit value object: exempt from the heuristic track.
		case inDomain && hasUnexportedField[name] && hasValueTransition[name]:
			checkHeuristicRoot(pass, at, methods[name])
		}
	}

	return nil, nil //nolint:nilnil
}

// checkMarkedRoot enforces the encapsulation rules on an explicitly
// marked aggregate root: no exported mutable fields and no unguarded
// pointer transitions.
func checkMarkedRoot(pass *analysis.Pass, at *aggType, unguarded []member) {
	for _, f := range exportedFields(at.st) {
		pass.Reportf(f.pos,
			"aggregate root %q exposes exported mutable field %q; encapsulate it behind a read-only accessor and a guarded mutator so the aggregate owns its invariants",
			at.name, f.name)
	}
	for _, m := range unguarded {
		pass.Reportf(m.pos,
			"aggregate root %q has unguarded pointer transition %q; a state-mutating method must return a result (e.g. error) so invariant violations can be rejected",
			at.name, m.name)
	}
}

// checkHeuristicRoot enforces the legacy value-aggregate rules: the type
// must live in an _aggregate.go file and declare Validate() error.
func checkHeuristicRoot(pass *analysis.Pass, at *aggType, methodSet map[string]bool) {
	base := filepath.Base(at.file)
	if !strings.HasSuffix(base, "_aggregate.go") {
		pass.Reportf(at.pos,
			"aggregate-like type %q in %s should live in a file ending '_aggregate.go' (or carry a // aggregate-root marker) so its role is visible at a glance",
			at.name, base)
	}
	if !methodSet["Validate"] {
		pass.Reportf(at.pos,
			"aggregate-like type %q (in %s) must declare a Validate() error method to make invariants explicit",
			at.name, base)
	}
}

// hasMarker reports whether the comment group carries marker on a line of
// its own (so prose mentions never match). It checks both the normalised
// text returned by cg.Text() (handles "// marker" with a space) AND the
// raw individual comment texts (handles "//marker" without a space, which
// the Go AST treats as a pragma-style comment and omits from Text()).
func hasMarker(cg *ast.CommentGroup, marker string) bool {
	if cg == nil {
		return false
	}
	for _, line := range strings.Split(cg.Text(), "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	// Raw check for pragma-style "//marker" (no space after //).
	for _, c := range cg.List {
		raw := strings.TrimPrefix(c.Text, "//")
		if strings.TrimSpace(raw) == marker {
			return true
		}
	}
	return false
}

// hasUnexported returns true if the struct has at least one unexported
// field.
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

// exportedFields returns the exported named fields of the struct.
// Embedded (anonymous) fields have no names and are skipped.
func exportedFields(st *ast.StructType) []member {
	var out []member
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			if n.IsExported() {
				out = append(out, member{n.Name, n.Pos()})
			}
		}
	}
	return out
}

// receiverTypeName extracts the receiver type name and whether it is a
// pointer receiver. Returns ("", false) if the receiver shape is not a
// recognised T or *T.
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

// returnsSelf returns true if the function type returns the named value
// type as its first or only return value.
func returnsSelf(ft *ast.FuncType, typeName string) bool {
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return false
	}
	id, ok := ft.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == typeName
}

// returnsNothing reports whether the function declares no results.
func returnsNothing(ft *ast.FuncType) bool {
	return ft.Results == nil || len(ft.Results.List) == 0
}

// mutatesReceiver reports whether the method body assigns to (or
// increments) a field of its named receiver — i.e. it mutates aggregate
// state in place rather than only reading it.
func mutatesReceiver(fd *ast.FuncDecl) bool {
	if fd.Body == nil || fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	names := fd.Recv.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return false
	}
	recv := names[0].Name
	mutates := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range s.Lhs {
				if isReceiverField(lhs, recv) {
					mutates = true
				}
			}
		case *ast.IncDecStmt:
			if isReceiverField(s.X, recv) {
				mutates = true
			}
		}
		return !mutates
	})
	return mutates
}

// isReceiverField reports whether assigning to expr mutates state rooted at
// the named receiver. It catches the direct field (r.field), an element of a
// field (r.field[i], r.m[key]), a sub-field (r.field.sub), and a pointer-field
// dereference (*r.field) — index/deref/selector wrappers around a receiver
// field still mutate the aggregate's own state, so a void method doing any of
// them is an unguarded transition unless explicitly allow-listed.
func isReceiverField(expr ast.Expr, recv string) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && id.Name == recv {
			return true // r.field
		}
		return isReceiverField(e.X, recv) // r.field.sub → recurse on r.field
	case *ast.IndexExpr:
		return isReceiverField(e.X, recv) // r.field[i] / r.m[key]
	case *ast.IndexListExpr:
		return isReceiverField(e.X, recv)
	case *ast.StarExpr:
		return isReceiverField(e.X, recv) // *r.field
	case *ast.ParenExpr:
		return isReceiverField(e.X, recv) // (r.field)
	}
	return false
}
