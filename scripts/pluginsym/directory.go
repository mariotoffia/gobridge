package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type fileKinds struct {
	path       string
	registered []string
	wired      map[string]bool
}

// analyzeDirectory checks source files without evaluating the host's build tags.
// Symmetry within each independently selected file makes family combinations safe.
func analyzeDirectory(dir string) ([]fileKinds, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read composition directory %q: %w", dir, err)
	}
	var files []fileKinds
	var violations []string
	owners := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse composition file %q: %w", path, err)
		}
		paths, err := parseRegisteredAdapterPaths(path)
		if err != nil {
			return nil, nil, err
		}
		for _, adapter := range paths {
			if owner, ok := owners[adapter]; ok {
				violations = append(violations, fmt.Sprintf("%s: R2: adapter %q already registers in %s", path, adapter, owner))
			} else {
				owners[adapter] = path
			}
		}
		registered, err := buildRegisteredKinds(path)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s: R1: %v", path, err))
		}
		wired, err := parseWiredKinds(path)
		if err != nil {
			return nil, nil, err
		}
		hasCalls, callViolations := checkRegistrationCalls(fset, f)
		violations = append(violations, callViolations...)
		violations = append(violations, checkFileConstraint(path, f, hasCalls)...)
		for _, failure := range checkSymmetry(registered, wired, aliasMap) {
			violations = append(violations, fmt.Sprintf("%s: R1: %s", path, strings.TrimSpace(failure)))
		}
		files = append(files, fileKinds{path: path, registered: registered, wired: wired})
	}
	return files, violations, nil
}

// Reject indirect uses rather than silently losing their kinds from the AST.
func checkRegistrationCalls(fset *token.FileSet, f *ast.File) (bool, []string) {
	direct := map[*ast.SelectorExpr]*ast.CallExpr{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				direct[sel] = call
			}
		}
		return true
	})
	imports := fileImports(f)
	hasCalls := false
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		wiring := wiringMethods[sel.Sel.Name]
		adapterRegister := false
		if id, ok := sel.X.(*ast.Ident); ok {
			adapterRegister = sel.Sel.Name == "Register" && strings.Contains(imports[id.Name], "/adapters/")
		}
		if !wiring && !adapterRegister {
			return true
		}
		hasCalls = true
		call, ok := direct[sel]
		if !ok {
			violations = append(violations, fmt.Sprintf("%s: R1: %s must be called directly, not used as a function value",
				fset.Position(sel.Pos()), sel.Sel.Name))
			return true
		}
		if !wiring {
			return true
		}
		if len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				return true
			}
		}
		violations = append(violations, fmt.Sprintf("%s: R1: %s kind must be a string literal",
			fset.Position(call.Pos()), sel.Sel.Name))
		return true
	})
	return hasCalls, violations
}
