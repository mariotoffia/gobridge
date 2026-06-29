// cfgshape implements the typed-pluggable-config rules from
// FIX-003. It enforces four properties, each scoped to a different
// part of the workspace:
//
//  1. Inner-ring (ports/, domain/) carriers: exported struct fields,
//     function parameters, and named return values whose name is in
//     the canonical "untyped config" set (Config, Options, Settings,
//     Extra, Raw) MUST NOT be typed as map[string]any,
//     map[string]interface{}, json.RawMessage, *structpb.Struct, or
//     unbounded []byte. Such carriers smuggle untyped configuration
//     into the inner ring and defeat the purpose of ports.PluginConfig.
//
//  2. Adapter shape: every plugin adapter package (a package that
//     opted into the plugin contract by either exposing an exported
//     Register function on a register.go file or by exporting a
//     type that satisfies ports.PluginConfig) MUST do BOTH:
//     contain a register.go that declares an exported
//     Register(reg *ports.Registry) error function, AND export at
//     least one type that satisfies ports.PluginConfig.
//     Half-baked plugins (registered but untyped, or typed but not
//     registered) fail this rule.
//
//  3. Validate body: every method named Validate on a type that
//     satisfies ports.PluginConfig MUST have a non-empty body, so a
//     typed plugin config declares its invariants explicitly rather
//     than rubber-stamping them with an empty method.
//
//     NOTE: this rule enforces ONLY the non-empty body. An earlier
//     design also required a same-package *_test.go to reference the
//     Validate method textually, but that test-reference check is
//     intentionally NOT enforced: vet's pass.Files excludes test
//     files, and the directory-grep heuristic produced too many false
//     positives on configs exercised indirectly through factories
//     rather than via a direct .Validate() call. It is deliberately
//     left disabled rather than re-enabled as a flaky gate.
//
//  4. Secret fields: a type satisfying ports.PluginConfig (and any
//     same-package struct reachable from it, including nested role and
//     connection sub-configs) MUST NOT expose an EXPORTED string field
//     whose name matches the secret-name heuristic (password, secret,
//     connectionstring, apikey, accesskey, privatekey, passphrase, a
//     *token suffix, or a TLS PEM/private-key suffix: *PEM, *KeyPEM,
//     *PrivateKey, *ClientKey). Such fields must use shared.Secret or
//     another non-string redaction wrapper so credentials cannot leak
//     through logs, JSON, or fmt. The heuristic is deliberately
//     conservative — no bare "key", and "token" matches only as a whole
//     word or suffix — so RoutingKey, PartitionKey, and TokenBucket are
//     never flagged.
//
// Exemptions:
//   - All *_test.go files (vet excludes them by default; we re-check).
//   - Packages whose path contains /acl/ or ends in /acl (vendor
//     anti-corruption layers).
//   - The arch-lint exempt packages domain/clock and ports/storetest
//     (and their sub-packages) — they are inner-ring helpers.
//   - Pure-test packages (no non-test files in pass.Files).
//
// The analyzer constructs ports.PluginConfig synthetically via
// types.NewInterfaceType so it does not need to resolve the actual
// import — the analyzer module is a separate Go module and depending
// on the main module would create a cycle.
package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the singleton cfgshape analysis pass.
var Analyzer = &analysis.Analyzer{
	Name: "cfgshape",
	Doc:  "enforces typed pluggable config shapes (no untyped carriers in ports/ + domain/; well-formed adapter plugin packages; non-empty Validate bodies)",
	Run:  run,
}

// untypedCarrierNames is the set of exported field/parameter/result
// names that are forbidden from carrying an untyped payload in the
// inner ring. The names match the historical "junk drawer" patterns
// the typed-plugin-config refactor removed.
var untypedCarrierNames = map[string]struct{}{
	"Config":   {},
	"Options":  {},
	"Settings": {},
	"Extra":    {},
	"Raw":      {},
}

// pluginConfigIface is a synthetic *types.Interface mirroring
// ports.PluginConfig (Kind() string + Validate() error). Built once
// on first use and cached package-globally.
var pluginConfigIface = buildPluginConfigInterface()

func buildPluginConfigInterface() *types.Interface {
	errType := types.Universe.Lookup("error").Type()
	kindSig := types.NewSignatureType(nil, nil, nil, nil,
		types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)
	validateSig := types.NewSignatureType(nil, nil, nil, nil,
		types.NewTuple(types.NewVar(token.NoPos, nil, "", errType)), false)
	kind := types.NewFunc(token.NoPos, nil, "Kind", kindSig)
	validate := types.NewFunc(token.NoPos, nil, "Validate", validateSig)
	iface := types.NewInterfaceType([]*types.Func{kind, validate}, nil)
	iface.Complete()
	return iface
}

func run(pass *analysis.Pass) (interface{}, error) {
	pkgPath := pass.Pkg.Path()
	if isExemptPackage(pkgPath) {
		return nil, nil //nolint:nilnil
	}

	switch {
	case isInnerRingPath(pkgPath):
		checkInnerRing(pass)
	case isAdapterLeafPath(pkgPath):
		checkAdapter(pass)
	}

	// Validate-body check applies wherever a PluginConfig type is
	// declared, which in this codebase only happens in adapter leaves,
	// but we run it unconditionally so a future inner-ring PluginConfig
	// (unlikely) would still be enforced. The test-reference check is
	// intentionally disabled (see checkValidateMethods); only the
	// non-empty Validate body is enforced here.
	checkValidateMethods(pass)

	// Secret-handling check: a PluginConfig (and any same-package struct
	// reachable from it) MUST NOT expose a raw secret-looking string
	// field — those must use shared.Secret or another redaction wrapper.
	checkSecretFields(pass)

	return nil, nil //nolint:nilnil
}

// isExemptPackage returns true for paths the analyzer must not flag.
//
// We exempt arch-lint-exempt inner-ring helpers and any package whose
// path lives inside an /acl/ sub-tree (vendor ACLs are allowed to
// hold loosely-typed payloads at the SDK boundary).
func isExemptPackage(path string) bool {
	if strings.Contains(path, "/acl/") || strings.HasSuffix(path, "/acl") {
		return true
	}
	if strings.HasSuffix(path, "/domain/clock") || strings.Contains(path, "/domain/clock/") {
		return true
	}
	if strings.HasSuffix(path, "/ports/storetest") || strings.Contains(path, "/ports/storetest/") {
		return true
	}
	return false
}

// isInnerRingPath returns true for ports/ or domain/ packages.
func isInnerRingPath(path string) bool {
	return strings.HasSuffix(path, "/ports") || strings.Contains(path, "/ports/") ||
		strings.HasSuffix(path, "/domain") || strings.Contains(path, "/domain/")
}

// isAdapterLeafPath returns true for adapter packages (any package
// under /adapters/).
func isAdapterLeafPath(path string) bool {
	return strings.Contains(path, "/adapters/")
}

// nonTestFiles returns the non-test files in pass.Files.
func nonTestFiles(pass *analysis.Pass) []*ast.File {
	out := make([]*ast.File, 0, len(pass.Files))
	for _, f := range pass.Files {
		name := pass.Fset.Position(f.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ----------------------------------------------------------------------
// Inner-ring rule
// ----------------------------------------------------------------------

func checkInnerRing(pass *analysis.Pass) {
	for _, f := range nonTestFiles(pass) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				// Type declarations only — fields on top-level named
				// structs are the API surface we care about. Anonymous
				// inline structs (e.g. JSON marshalling helpers) are
				// implementation detail and exempt.
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch t := ts.Type.(type) {
					case *ast.StructType:
						if t.Fields == nil {
							continue
						}
						for _, fld := range t.Fields.List {
							checkInnerRingField(pass, fld, "field")
						}
					case *ast.FuncType:
						checkFieldList(pass, t.Params, "parameter")
						checkFieldList(pass, t.Results, "return")
					case *ast.InterfaceType:
						if t.Methods == nil {
							continue
						}
						for _, m := range t.Methods.List {
							ft, ok := m.Type.(*ast.FuncType)
							if !ok {
								continue
							}
							checkFieldList(pass, ft.Params, "parameter")
							checkFieldList(pass, ft.Results, "return")
						}
					}
				}
			case *ast.FuncDecl:
				// Only exported top-level functions/methods participate
				// in the inner-ring API surface.
				if d.Name == nil || !d.Name.IsExported() {
					continue
				}
				if d.Type == nil {
					continue
				}
				checkFieldList(pass, d.Type.Params, "parameter")
				checkFieldList(pass, d.Type.Results, "return")
			}
		}
	}
}

func checkInnerRingField(pass *analysis.Pass, fld *ast.Field, kind string) {
	if !isUntypedCarrierType(fld.Type) {
		return
	}
	for _, name := range fld.Names {
		if !name.IsExported() {
			continue
		}
		if _, hit := untypedCarrierNames[name.Name]; hit {
			pass.Reportf(name.Pos(),
				"cfgshape: %s %s of forbidden type %s in inner-ring package %s violates typed-plugin-config rule (use a typed ports.PluginConfig)",
				kind, name.Name, renderType(fld.Type), pass.Pkg.Path())
		}
	}
}

func checkFieldList(pass *analysis.Pass, fl *ast.FieldList, kind string) {
	if fl == nil {
		return
	}
	for _, fld := range fl.List {
		if !isUntypedCarrierType(fld.Type) {
			continue
		}
		for _, name := range fld.Names {
			if !name.IsExported() {
				continue
			}
			if _, hit := untypedCarrierNames[name.Name]; hit {
				pass.Reportf(name.Pos(),
					"cfgshape: %s %s of forbidden type %s in inner-ring package %s violates typed-plugin-config rule (use a typed ports.PluginConfig)",
					kind, name.Name, renderType(fld.Type), pass.Pkg.Path())
			}
		}
	}
}

// isUntypedCarrierType returns true if expr matches one of the
// forbidden types: map[string]any, map[string]interface{},
// json.RawMessage, *structpb.Struct, or unbounded []byte.
func isUntypedCarrierType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.MapType:
		key, ok := t.Key.(*ast.Ident)
		if !ok || key.Name != "string" {
			return false
		}
		switch v := t.Value.(type) {
		case *ast.Ident:
			return v.Name == "any"
		case *ast.InterfaceType:
			return v.Methods == nil || len(v.Methods.List) == 0
		}
		return false
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		if x.Name == "json" && t.Sel.Name == "RawMessage" {
			return true
		}
		return false
	case *ast.StarExpr:
		sel, ok := t.X.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		return x.Name == "structpb" && sel.Sel.Name == "Struct"
	case *ast.ArrayType:
		if t.Len != nil {
			return false
		}
		id, ok := t.Elt.(*ast.Ident)
		return ok && id.Name == "byte"
	}
	return false
}

func renderType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.MapType:
		return "map[" + renderType(t.Key) + "]" + renderType(t.Value)
	case *ast.Ident:
		return t.Name
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "interface{}"
		}
		return "interface{...}"
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
		return "<sel>." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + renderType(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + renderType(t.Elt)
		}
		return "[N]" + renderType(t.Elt)
	}
	return "<?>"
}

// ----------------------------------------------------------------------
// Adapter-shape rule
// ----------------------------------------------------------------------

func checkAdapter(pass *analysis.Pass) {
	nonTest := nonTestFiles(pass)
	if len(nonTest) == 0 {
		// pure-test package — nothing to enforce.
		return
	}

	hasPluginType := adapterExportsPluginConfig(pass)
	regFile, hasRegisterCall := findRegisterFile(pass, nonTest)

	// "Opt-in" gate: a package is a plugin only if it shows at least
	// one signal of engaging with the plugin contract. Helper packages
	// (acl-only, infra) that do neither are skipped to keep the
	// analyzer from flagging legitimate non-plugin adapters.
	if !hasPluginType && regFile == "" {
		return
	}

	if !hasPluginType {
		pass.Reportf(token.NoPos,
			"cfgshape: adapter package %s declares Register but exports no type satisfying ports.PluginConfig (Kind() string + Validate() error)",
			pass.Pkg.Path())
	}
	if regFile == "" {
		pass.Reportf(token.NoPos,
			"cfgshape: adapter package %s exports a ports.PluginConfig type but is missing a register.go that declares Register(reg *ports.Registry) error",
			pass.Pkg.Path())
		return
	}
	if !hasRegisterCall {
		pass.Reportf(token.NoPos,
			"cfgshape: adapter package %s has %s but it does not declare an exported Register function",
			pass.Pkg.Path(), filepath.Base(regFile))
	}
}

// adapterExportsPluginConfig returns true if the package declares an
// exported, non-interface named type whose method set satisfies
// ports.PluginConfig (synthetic interface).
func adapterExportsPluginConfig(pass *analysis.Pass) bool {
	scope := pass.Pkg.Scope()
	for _, name := range scope.Names() {
		if !ast.IsExported(name) {
			continue
		}
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
			return true
		}
	}
	return false
}

// findRegisterFile returns (filename, sawRegisterDecl). filename is
// "" if no register.go exists in the package.
func findRegisterFile(pass *analysis.Pass, files []*ast.File) (string, bool) {
	for _, f := range files {
		path := pass.Fset.Position(f.Pos()).Filename
		if filepath.Base(path) != "register.go" {
			continue
		}
		return path, fileDeclaresExportedRegister(f)
	}
	return "", false
}

// fileDeclaresExportedRegister returns true if the AST contains an
// exported top-level function declaration named "Register". The
// signature shape (Register(reg *ports.Registry) error) is enforced
// by the Go compiler at adapter call sites; the analyzer only
// asserts the declaration exists.
func fileDeclaresExportedRegister(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil {
			continue
		}
		if fn.Name.Name == "Register" {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// Validate-method rule lives in analyzer_validate.go.
// ----------------------------------------------------------------------
