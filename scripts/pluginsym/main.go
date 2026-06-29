// Command pluginsym enforces plugin-registry symmetry between the
// per-process *ports.Registry (config-decoder side) and the
// composition root that wires concrete factories into the
// *bridge.Supervisor / *bridge.Builder (factory side).
//
// Asymmetry is the AP-008 anti-pattern: an adapter registers a
// PluginConfig decoder for kind "foo" but no factory is ever wired
// (or vice-versa). The runtime then fails at composition time with
// an unhelpful error, and CI never noticed.
//
// For each adapter the canonical composition root invokes Register
// for, this tool asserts:
//
//   - Every kind that has a registered ConfigDecoder has a
//     corresponding wired factory in the composition root. Aliases
//     (e.g. "sqs" / "aws.sqs", "mqtt" / "mqtt.paho") collapse via
//     aliasMap onto a canonical kind, and the canonical-kind group
//     is satisfied if at least one alias is wired.
//   - Every wired factory in the composition root corresponds to a
//     registered config decoder (no orphan factories).
//
// Usage:
//
//	go run ./scripts/pluginsym
//	go run ./scripts/pluginsym -v
//	go run ./scripts/pluginsym -main <path/to/main.go>
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	// Adapter Register functions are invoked through the
	// adapterRegistrars binding below. WHICH adapters are invoked is
	// PARSED from the composition root (cmd/gobridge/main.go) at run
	// time, so this import set only has to provide the compile-time
	// binding for every adapter the composition root may register.
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"

	"github.com/mariotoffia/gobridge/ports"
)

// aliasMap collapses adapter-registered alias kinds onto their
// canonical wired-factory kind. Adapters that register both a short
// ("mqtt") and fully-qualified ("mqtt.paho") form land here so the
// symmetry check reports against a single canonical entry. The set
// is curated and matches the alias conventions used in
// scripts/registrychk and the supervisor's transport/store maps in
// cmd/gobridge/main.go.
var aliasMap = map[string]string{
	"aws.sqs":   "sqs",
	"mqtt.paho": "mqtt",
}

// wiringMethods is the set of Supervisor/Builder method names whose
// first string-literal argument is the kind under which a factory
// is wired. Both Supervisor (RegisterTransport, RegisterStoreFactory)
// and Builder (RegisterTransportFactory, RegisterStoreFactory) APIs
// are accepted so the same analyzer covers both composition styles.
var wiringMethods = map[string]bool{
	"RegisterTransport":        true, // *bridge.Supervisor
	"RegisterTransportFactory": true, // *bridge.Builder
	"RegisterStoreFactory":     true, // both
}

func main() {
	var (
		mainPath = flag.String("main", "", "path to the canonical composition root main.go (default: derived from cwd as cmd/gobridge/main.go)")
		verbose  = flag.Bool("v", false, "print discovered registered/wired kinds")
	)
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pluginsym: getwd: %v\n", err)
		os.Exit(2)
	}
	if *mainPath == "" {
		*mainPath = filepath.Join(cwd, "cmd", "gobridge", "main.go")
	}

	registered, err := buildRegisteredKinds(*mainPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pluginsym: build registry: %v\n", err)
		os.Exit(2)
	}

	wired, err := parseWiredKinds(*mainPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pluginsym: parse %s: %v\n", *mainPath, err)
		os.Exit(2)
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "pluginsym: registered kinds: %v\n", registered)
		fmt.Fprintf(os.Stderr, "pluginsym: wired kinds:      %v\n", sortedKeys(wired))
	}

	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) == 0 {
		return
	}
	for _, m := range missing {
		fmt.Fprint(os.Stderr, m)
	}
	os.Exit(1)
}

// adapterRegistrars binds each adapter package import path to its
// Register function. WHICH adapters get invoked is parsed from the
// composition root (see parseRegisteredAdapterPaths); this map only
// supplies the compile-time binding Go's static linking requires. A
// Register call in the composition root whose import path is absent
// here is a drift error — add the adapter here and to go.mod.
var adapterRegistrars = map[string]func(*ports.Registry) error{
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho": paho.Register,
	"github.com/mariotoffia/gobridge/adapters/native/store":        nativestore.Register,
}

// buildRegisteredKinds parses the composition root at mainPath for the
// adapter Register(reg) calls it makes, invokes each via the
// adapterRegistrars binding, and returns the sorted registered kinds.
// The set therefore mirrors the real composition root automatically
// instead of a hand-maintained list. A Register call for an adapter not
// present in adapterRegistrars is reported as drift.
func buildRegisteredKinds(mainPath string) ([]string, error) {
	paths, err := parseRegisteredAdapterPaths(mainPath)
	if err != nil {
		return nil, err
	}
	reg := ports.NewRegistry()
	var errs []error
	for _, p := range paths {
		register, ok := adapterRegistrars[p]
		if !ok {
			return nil, fmt.Errorf(
				"composition root %s registers adapter %q but pluginsym has no binding for it; add it to adapterRegistrars (and go.mod)",
				mainPath, p)
		}
		errs = append(errs, register(reg))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("register adapters: %w", err)
	}
	return reg.Kinds(), nil
}

// parseRegisteredAdapterPaths parses the composition-root file at path
// and returns the import paths of every adapter whose Register(reg) it
// invokes — i.e. selector calls `pkg.Register(...)` whose pkg resolves
// to an imported "/adapters/" package. Calls on a non-package receiver
// (e.g. reg.Register(kind, dec)) are ignored because the receiver does
// not resolve to an imported adapter path.
func parseRegisteredAdapterPaths(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	imports := fileImports(f)
	seen := map[string]bool{}
	var paths []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Register" {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pkgPath, ok := imports[id.Name]
		if !ok || !strings.Contains(pkgPath, "/adapters/") {
			return true
		}
		if !seen[pkgPath] {
			seen[pkgPath] = true
			paths = append(paths, pkgPath)
		}
		return true
	})
	sort.Strings(paths)
	return paths, nil
}

// fileImports maps each import's local name to its import path.
func fileImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// parseWiredKinds parses the supplied main.go and returns the set of
// kind strings passed as the first argument to any of wiringMethods.
// Non-literal arguments (variables, constants from other packages)
// are skipped with no error — the analyzer only enforces what it
// can statically observe; dynamic wiring is out of scope.
func parseWiredKinds(path string) (map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		if !wiringMethods[sel.Sel.Name] {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out[s] = true
		return true
	})
	return out, nil
}

// canonicalize collapses an alias kind onto its canonical wired-
// factory name via aliasMap. Kinds without an alias entry are
// returned unchanged.
func canonicalize(kind string, aliases map[string]string) string {
	if c, ok := aliases[kind]; ok {
		return c
	}
	return kind
}

// canonicalSet returns the deduplicated, sorted set of canonical
// kinds derived from the supplied alias-bearing kinds. Each input
// kind is collapsed via canonicalize.
func canonicalSet(kinds []string, aliases map[string]string) []string {
	seen := map[string]bool{}
	for _, k := range kinds {
		seen[canonicalize(k, aliases)] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// aliasesOf returns every kind that canonicalizes to canon, including
// canon itself. The result is sorted for stable diagnostics.
func aliasesOf(canon string, aliases map[string]string) []string {
	out := []string{canon}
	for alias, target := range aliases {
		if target == canon {
			out = append(out, alias)
		}
	}
	sort.Strings(out)
	return out
}

// checkSymmetry runs the bidirectional symmetry assertion and returns
// one human-readable error block per asymmetric kind. An empty return
// value means the registry and the composition root are symmetric.
//
// Direction A (decoder without factory): every canonical kind that
// has a registered decoder must have at least one of its aliases
// wired in the composition root.
//
// Direction B (factory without decoder): every wired factory kind
// must canonicalize to a kind that has a registered decoder.
func checkSymmetry(registered []string, wired map[string]bool, aliases map[string]string) []string {
	var out []string

	canonRegistered := canonicalSet(registered, aliases)
	registeredSet := map[string]bool{}
	for _, k := range registered {
		registeredSet[k] = true
	}

	// Direction A: decoder-side kinds without a wired factory.
	for _, canon := range canonRegistered {
		group := aliasesOf(canon, aliases)
		anyWired := false
		for _, a := range group {
			if wired[a] {
				anyWired = true
				break
			}
		}
		if anyWired {
			continue
		}
		out = append(out, fmt.Sprintf(
			"pluginsym: registered kind %q has no wired factory in the composition root\n"+
				"  - registered aliases: %v\n"+
				"  - expected one of:    Supervisor.RegisterTransport / RegisterStoreFactory or Builder.RegisterTransportFactory / RegisterStoreFactory keyed by one of the aliases above\n",
			canon, group,
		))
	}

	// Direction B: wired factory kinds without a registered decoder.
	for _, name := range sortedKeys(wired) {
		canon := canonicalize(name, aliases)
		// Either the wired name itself, the canonical form, or any
		// alias of the canonical form must exist in the registered
		// set for the wiring to be backed by a decoder.
		if registeredSet[name] || registeredSet[canon] {
			continue
		}
		matched := false
		for _, a := range aliasesOf(canon, aliases) {
			if registeredSet[a] {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		out = append(out, fmt.Sprintf(
			"pluginsym: wired factory kind %q has no registered config decoder\n"+
				"  - canonical kind:    %q\n"+
				"  - expected:          an adapter Register(reg) call that registers a ConfigDecoder for %q (or one of its aliases) in the composition root\n",
			name, canon, canon,
		))
	}

	return out
}

// sortedKeys returns the keys of m as a sorted slice. Used to keep
// diagnostic output stable across runs.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
