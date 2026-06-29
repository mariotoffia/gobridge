// aclcheck implements a go/analysis pass that enforces the hexagonal
// anti-corruption layer convention in adapter packages.
//
// Inside an adapter package directory (any path containing "/adapters/")
// the ACL boundary is the only place allowed to speak the vendor SDK's
// vocabulary. The pass enforces TWO complementary rules:
//
//  1. Import confinement. The only files allowed to import a vendor SDK
//     package are ACL files — files whose name matches acl*.go (e.g.
//     acl_envelope.go) or files in an acl/ sub-directory. Any other
//     (non-test) file importing a vendor SDK is flagged.
//
//  2. Export confinement (type-origin). An ACL file may import the SDK,
//     but it MUST NOT re-export an SDK type under an EXPORTED name. An
//     exported type alias (type Foo = sdk.Bar) or an exported defined
//     type whose source is an SDK type (type Foo sdk.Bar, including
//     pointer/slice/map/chan wrappers of one) leaks the SDK type past
//     the boundary: non-ACL files can then name and pass the SDK type
//     freely, defeating rule 1. The same applies to function and method
//     signatures: an exported package-level func, or a method on an
//     exported receiver, whose params or results name an SDK type leaks
//     it just as plainly. Such declarations are flagged unless the
//     export is intentional and annotated with a directive comment:
//
//     //aclcheck:allow-export
//
//     The correct ACL pattern is to wrap the SDK value in a struct with
//     UNEXPORTED fields and expose only domain-typed accessors, so the
//     SDK type never appears in the package's exported surface.
//
// The analyzer is configured with a list of "vendor SDK" import-path
// prefixes (vendorPatterns); extend it when adding a new adapter
// technology. Test files are exempt from both rules: tests legitimately
// construct SDK types directly.
//
// Why this rule: the anti-corruption layer is the seam between an
// external SDK's vocabulary and the project's domain types.
// Concentrating that seam — both the imports AND the exported surface —
// in explicitly-named files keeps the SDK boundary visible and prevents
// it from silently leaking across the adapter.
package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// vendorPatterns lists the import-path prefixes the analyzer treats
// as "vendor SDK" imports for ACL purposes. Extend this list when
// adding a new transport, store, or observability adapter that
// brings in a new SDK family.
var vendorPatterns = []string{
	"github.com/aws/aws-sdk-go-v2/",
	"github.com/aws/smithy-go",
	"github.com/Azure/",
	"github.com/eclipse/paho.golang/",
	"github.com/rabbitmq/amqp091-go",
	"github.com/fsnotify/fsnotify",
	"go.opentelemetry.io/",
	"modernc.org/sqlite",
}

// allowExportDirective is the comment that opts an exported,
// SDK-derived type out of the export-confinement rule.
const allowExportDirective = "aclcheck:allow-export"

// Analyzer is the singleton aclcheck analysis pass.
var Analyzer = &analysis.Analyzer{
	Name: "aclcheck",
	Doc:  "checks that vendor SDK imports and SDK-derived exported types stay confined to ACL files in adapter packages",
	Run:  run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	// Only check adapter packages.
	if !strings.Contains(pass.Pkg.Path(), "/adapters/") {
		return nil, nil //nolint:nilnil // analysis.Run convention: nil result + nil err = no findings.
	}
	for _, f := range pass.Files {
		filename := pass.Fset.Position(f.Pos()).Filename
		if isTestFile(filename) {
			continue
		}
		if isACLFile(filename) {
			// ACL files may import the SDK but must not re-export its
			// types under an exported name.
			checkExports(pass, f)
			continue
		}
		// Non-ACL files must not import the SDK at all.
		checkImports(pass, f)
	}
	return nil, nil //nolint:nilnil
}

// checkImports flags every vendor SDK import in a non-ACL file.
func checkImports(pass *analysis.Pass, f *ast.File) {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !isVendorSDK(path) {
			continue
		}
		pass.Reportf(imp.Pos(),
			"vendor SDK import %q is allowed only in ACL files (acl_*.go) or acl/ directories; move the SDK boundary into an ACL", path)
	}
}

// checkExports flags exported declarations in an ACL file whose surface
// originates from a vendor SDK, unless the declaration carries the
// //aclcheck:allow-export directive. Two declaration kinds leak the SDK
// across the boundary:
//
//   - Type declarations whose underlying type is SDK-derived.
//   - Function/method signatures (params or results) that name an SDK
//     type, but only when reachable from outside the package: a
//     package-level exported func, or a method on an exported receiver
//     type. Such a signature lets non-ACL callers name and pass the SDK
//     type, defeating import confinement just like an exported alias.
func checkExports(pass *analysis.Pass, f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				checkTypeDecl(pass, d)
			}
		case *ast.FuncDecl:
			checkFuncDecl(pass, d)
		}
	}
}

// checkTypeDecl flags exported SDK-derived type declarations.
func checkTypeDecl(pass *analysis.Pass, gd *ast.GenDecl) {
	for _, spec := range gd.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok || !ts.Name.IsExported() {
			continue
		}
		if hasAllowExport(gd.Doc) || hasAllowExport(ts.Doc) || hasAllowExport(ts.Comment) {
			continue
		}
		origin := sdkOrigin(pass.TypesInfo.TypeOf(ts.Type))
		if origin == "" {
			continue
		}
		kind := "exported type"
		if ts.Assign.IsValid() {
			kind = "exported type alias"
		}
		pass.Reportf(ts.Pos(),
			"%s %q re-exports SDK type from %q across the ACL boundary; wrap the SDK value in a struct with unexported fields, or annotate with //aclcheck:allow-export if the export is intentional",
			kind, ts.Name.Name, origin)
	}
}

// checkFuncDecl flags an externally-reachable function/method whose
// signature (params or results) names a vendor SDK type. Unexported funcs
// and methods on unexported receivers cannot be named from outside the
// package, so they are not part of the leaking surface.
func checkFuncDecl(pass *analysis.Pass, fd *ast.FuncDecl) {
	if !fd.Name.IsExported() || !exportedReceiver(fd.Recv) {
		return
	}
	if hasAllowExport(fd.Doc) {
		return
	}
	ft := fd.Type
	origin := sdkOriginFields(pass, ft.Params)
	if origin == "" {
		origin = sdkOriginFields(pass, ft.Results)
	}
	if origin == "" {
		return
	}
	pass.Reportf(fd.Pos(),
		"exported function %q exposes SDK type from %q in its signature across the ACL boundary; return a domain type, or annotate with //aclcheck:allow-export if the export is intentional",
		fd.Name.Name, origin)
}

// exportedReceiver reports whether a method receiver is exported (so the
// method is reachable from outside the package). A nil receiver list means
// a package-level function, which is reachable when exported.
func exportedReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return true
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.IsExported()
	}
	return false
}

// sdkOriginFields returns the first SDK origin found across a field list's
// types (each parameter/result entry), or "" if none is SDK-derived.
func sdkOriginFields(pass *analysis.Pass, fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	for _, field := range fl.List {
		if origin := sdkOrigin(pass.TypesInfo.TypeOf(field.Type)); origin != "" {
			return origin
		}
	}
	return ""
}

// sdkOrigin returns the vendor SDK import path that t originates from, or
// "" if t is not SDK-derived. It unwraps pointer/slice/array/map/chan so
// an exported `type Foo []sdk.Bar` leaks just like a direct alias, but it
// does NOT descend into struct fields — a struct that keeps SDK values in
// unexported fields is the correct ACL pattern, not a leak.
func sdkOrigin(t types.Type) string {
	return sdkOriginType(t, map[types.Type]bool{})
}

func sdkOriginType(t types.Type, seen map[types.Type]bool) string {
	if t == nil || seen[t] {
		return ""
	}
	seen[t] = true
	switch u := types.Unalias(t).(type) {
	case *types.Named:
		if obj := u.Obj(); obj != nil && obj.Pkg() != nil {
			if p := obj.Pkg().Path(); isVendorSDK(p) {
				return p
			}
		}
		return ""
	case *types.Pointer:
		return sdkOriginType(u.Elem(), seen)
	case *types.Slice:
		return sdkOriginType(u.Elem(), seen)
	case *types.Array:
		return sdkOriginType(u.Elem(), seen)
	case *types.Chan:
		return sdkOriginType(u.Elem(), seen)
	case *types.Map:
		if p := sdkOriginType(u.Key(), seen); p != "" {
			return p
		}
		return sdkOriginType(u.Elem(), seen)
	}
	return ""
}

// hasAllowExport reports whether the comment group carries the
// //aclcheck:allow-export directive. Directive-style comments are scanned
// raw because go/ast strips them from CommentGroup.Text().
func hasAllowExport(cg *ast.CommentGroup) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if strings.Contains(c.Text, allowExportDirective) {
			return true
		}
	}
	return false
}

// isTestFile returns true for Go test files, which are exempt from both
// rules: tests legitimately construct SDK types directly (mocks,
// fixtures, edge-case probes).
func isTestFile(filename string) bool {
	return strings.HasSuffix(filepath.Base(filename), "_test.go")
}

// isACLFile returns true for ACL files — acl*.go by name or any file
// under an acl/ directory.
func isACLFile(filename string) bool {
	base := filepath.Base(filename)
	if strings.HasPrefix(base, "acl") {
		return true
	}
	return filepath.Base(filepath.Dir(filename)) == "acl"
}

// isVendorSDK returns true if the import path matches any of the
// known SDK prefixes the analyzer flags.
func isVendorSDK(path string) bool {
	for _, prefix := range vendorPatterns {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
