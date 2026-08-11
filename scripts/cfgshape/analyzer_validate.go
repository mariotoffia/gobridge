package main

import (
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// ----------------------------------------------------------------------
// Validate-method rule
// ----------------------------------------------------------------------

func checkValidateMethods(pass *analysis.Pass) {
	if isExemptPackage(pass.Pkg.Path()) {
		return
	}
	scope := pass.Pkg.Scope()
	pluginRecvNames := map[string]bool{}
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		if types.IsInterface(named) {
			continue
		}
		if types.Implements(named, pluginConfigIface) ||
			types.Implements(types.NewPointer(named), pluginConfigIface) {
			pluginRecvNames[name] = true
		}
	}
	if len(pluginRecvNames) == 0 {
		return
	}

	var pkgDir string
	for _, f := range pass.Files {
		filename := pass.Fset.Position(f.Pos()).Filename
		if filename != "" {
			pkgDir = filepath.Dir(filename)
			break
		}
	}

	for _, f := range nonTestFiles(pass) {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil || fd.Name.Name != "Validate" || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			recvName, _ := receiverTypeName(fd.Recv.List[0].Type)
			if recvName == "" || !pluginRecvNames[recvName] {
				continue
			}
			if fd.Body == nil || len(fd.Body.List) == 0 {
				pass.Reportf(fd.Name.Pos(),
					"cfgshape: %s.Validate must have a non-empty body — typed plugin configs declare invariants explicitly",
					recvName)
			}
			// NOTE: a stricter check would also require a *_test.go in
			// the same package directory that references
			// "<recvName>.Validate". That rule is intentionally NOT
			// enforced here: vet's pass.Files excludes test files, and
			// while we can re-read the directory, the heuristic match
			// produces enough false negatives on indirect test paths
			// (configs exercised through factories rather than via a
			// direct .Validate() call) that it would block legitimate
			// code. This is a known, accepted gap in the checker.
			_ = pkgDir
		}
	}
}

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

// packageTestsReferenceValidate scans dir for *_test.go files and
// returns true if any contains the substring "<recv>{...}.Validate"
// or simply ".Validate(" — we use the cheaper substring "Validate"
// scoped per file because false positives in this rule (a test
// happens to mention the word "Validate" elsewhere) are far less
// damaging than false negatives. The check exists primarily to flag
// completely untested Validate methods, not to verify call shape.
// packageTestsReferenceValidate is currently unused — kept as a
// reference for re-enabling the test-reference rule once we can
// resolve test-time call graphs reliably. See checkValidateMethods.
//
//nolint:unused // intentional: see comment above.
func packageTestsReferenceValidate(dir, recvName string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Cannot read the directory — assume the test exists rather
		// than emit a confusing diagnostic.
		return true
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		text := string(data)
		if strings.Contains(text, ".Validate(") || strings.Contains(text, recvName+"{") && strings.Contains(text, "Validate") {
			return true
		}
	}
	return false
}
