package main

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// ----------------------------------------------------------------------
// Secret-field rule
// ----------------------------------------------------------------------

// strongSecretSubstrings are case-insensitive substrings that, when
// found anywhere in an exported string field name, mark the field as
// carrying credential material. They are deliberately specific
// compounds (no bare "key") so non-secret fields like RoutingKey,
// PartitionKey, or CredentialsURIRef are never matched.
var strongSecretSubstrings = []string{
	"password",
	"passphrase",
	"secret", // Secret, ClientSecret, SecretKey, SecretAccessKey
	"connectionstring",
	"apikey",
	"accesskey", // AccessKey, SecretAccessKey, SharedAccessKey
	"sharedaccesskey",
	"saskey",
	"privatekey",
}

// pemSecretSuffixes mark an exported string field as carrying TLS
// private-key/cert PEM material (G2): a raw "KeyPEM"/"CertPEM"/
// "ClientKey" string leaks through logs/JSON/fmt like a password and
// must be wrapped in shared.Secret. "pem" covers *PEM/*KeyPEM; "clientkey"
// catches a bare client-key field. ("privatekey" is already a strong
// substring above.)
var pemSecretSuffixes = []string{"pem", "clientkey"}

// isSecretFieldName reports whether an exported field name looks like a
// secret per the conservative heuristic. The strong substrings match
// anywhere; "token" is weaker and matches only as a whole word or a
// suffix (AccessToken, AuthToken, SASToken) so prefixes like
// TokenBucket / Tokenizer are NOT flagged. PEM/private-key suffixes
// (KeyPEM, PEM, PrivateKey, ClientKey) are flagged too so raw TLS
// material strings must be wrapped in shared.Secret (G2).
func isSecretFieldName(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range strongSecretSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	for _, s := range pemSecretSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return lower == "token" || strings.HasSuffix(lower, "token")
}

// checkSecretFields flags raw secret-looking string fields on every
// type that satisfies ports.PluginConfig, recursing into same-package
// struct fields (and embedded structs) so secrets nested inside a
// role/connection sub-config are caught too. The fix is to wrap the
// field in shared.Secret (or another non-string redaction wrapper);
// any non-string field type passes.
func checkSecretFields(pass *analysis.Pass) {
	if isExemptPackage(pass.Pkg.Path()) {
		return
	}
	scope := pass.Pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		// Test fixtures (e.g. a fake plugin config used to exercise a
		// plaintext-secret scanner) legitimately declare raw secret-
		// string fields; the secret-field rule is a PRODUCTION rule,
		// consistent with this analyzer's other *_test.go exemptions.
		if pos := pass.Fset.Position(tn.Pos()); strings.HasSuffix(pos.Filename, "_test.go") {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || types.IsInterface(named) {
			continue
		}
		if !types.Implements(named, pluginConfigIface) &&
			!types.Implements(types.NewPointer(named), pluginConfigIface) {
			continue
		}
		reportSecretStringFields(pass, named, named.Obj().Name(), map[types.Type]bool{})
	}
}

// reportSecretStringFields walks the struct underlying t, reporting any
// exported secret-looking string field and recursing into reachable
// same-package struct fields. visited guards against cyclic types.
func reportSecretStringFields(pass *analysis.Pass, t types.Type, typeName string, visited map[types.Type]bool) {
	st, ok := t.Underlying().(*types.Struct)
	if !ok || visited[t] {
		return
	}
	visited[t] = true
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Embedded() && f.Exported() && isStringType(f.Type()) && isSecretFieldName(f.Name()) {
			pass.Reportf(f.Pos(),
				"cfgshape: plugin-config field %s.%s is a raw secret-looking string; use shared.Secret (or another approved redaction wrapper) so credentials cannot leak through logs, JSON, or fmt",
				typeName, f.Name())
			continue
		}
		if next, nextName, ok := samePkgStructTarget(pass, f); ok {
			reportSecretStringFields(pass, next, nextName, visited)
		}
	}
}

// isStringType reports whether t is exactly the predeclared string type.
func isStringType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.String
}

// samePkgStructTarget returns the struct type to recurse into for field
// f, together with a display name. It dereferences a single pointer and
// recurses only into struct types declared in the SAME package (or an
// anonymous inline struct). Foreign types — including the approved
// shared.Secret wrapper and SDK structs — are never recursed, which
// keeps the heuristic free of cross-package false positives.
func samePkgStructTarget(pass *analysis.Pass, f *types.Var) (types.Type, string, bool) {
	ft := f.Type()
	if p, ok := ft.(*types.Pointer); ok {
		ft = p.Elem()
	}
	switch tt := ft.(type) {
	case *types.Named:
		if tt.Obj().Pkg() != pass.Pkg {
			return nil, "", false
		}
		if _, ok := tt.Underlying().(*types.Struct); !ok {
			return nil, "", false
		}
		return tt, tt.Obj().Name(), true
	case *types.Struct:
		return tt, f.Name(), true
	}
	return nil, "", false
}
