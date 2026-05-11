// Command registrychk enforces that every AWS-deployable plugin kind
// registered into the CDK plugin registry has matching CDK helpers in
// the deployment/aws-filebased-config/cdk tree.
//
// For each AWS-deployable canonical kind it asserts:
//
//   - A bridgecfg builder symbol exists with prefix "With<Kind>*"
//     under deployment/aws-filebased-config/cdk/bridgecfg/.
//   - A grants helper file exists at
//     deployment/aws-filebased-config/cdk/constructs/internal/grants/
//     when the kind requires IAM grants.
//
// Pure non-AWS kinds (azure.*, amqp.*, amqp091, amqp10, servicebus)
// are skipped with a debug note (-v) — the CDK only deploys AWS.
//
// Usage:
//
//	go run ./scripts/registrychk
//	go run ./scripts/registrychk -v
//	go run ./scripts/registrychk -bridgecfg-dir <path> -grants-dir <path>
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
	"strings"

	// Adapter Register functions are invoked explicitly below to
	// populate the per-process registry the CDK can synthesise.
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"

	"github.com/mariotoffia/gobridge/ports"
)

// awsDeployableKinds is the curated set of canonical (post-alias)
// kinds that the file-based-config CDK is expected to deploy. The
// values describe what every kind needs:
//   - builderPrefix: every bridgecfg helper covering this kind must
//     be exported under "With<builderPrefix>*". A kind with builder
//     coverage exposes >=1 such symbol.
//   - grantsFile: relative filename inside the grants/ directory.
//     Empty string means the kind has no IAM surface (no grants
//     helper required) — currently true for transport-only kinds
//     (http, mqtt) and in-process stores (memory, sqlite).
type kindSpec struct {
	builderPrefix string
	grantsFile    string
}

var awsDeployableKinds = map[string]kindSpec{
	// Transports.
	"sqs":  {builderPrefix: "SQS", grantsFile: "sqs.go"},
	"http": {builderPrefix: "HTTP", grantsFile: ""},
	"mqtt": {builderPrefix: "MQTT", grantsFile: ""},
	// Stores.
	"memory":   {builderPrefix: "Memory", grantsFile: ""},
	"sqlite":   {builderPrefix: "SQLite", grantsFile: ""},
	"dynamodb": {builderPrefix: "DynamoDB", grantsFile: "dynamodb.go"},
}

// aliasMap collapses adapter-registered alias kinds onto their
// canonical AWS-deployable kind. Adapters that register both a
// short ("sqs") and fully qualified ("aws.sqs") form land here so
// the registrychk reports against a single canonical entry.
var aliasMap = map[string]string{
	"aws.sqs":   "sqs",
	"mqtt.paho": "mqtt",
}

// nonAWSPrefixes is the set of kind prefixes that mark a kind as
// non-AWS-deployable. The CDK has no construct surface for these so
// they are skipped (debug-logged under -v) rather than failing the
// gate.
var nonAWSPrefixes = []string{
	"azure.",
	"amqp.",
}

// nonAWSExact is the set of bare (non-prefixed) kinds that are still
// non-AWS-deployable. Adapters often register both a short and a
// long form; the short form lacks the prefix and must be matched by
// exact name here.
var nonAWSExact = map[string]bool{
	"servicebus": true,
	"amqp10":     true,
	"amqp091":    true,
}

func main() {
	var (
		bridgecfgDir = flag.String("bridgecfg-dir", "", "path to deployment/aws-filebased-config/cdk/bridgecfg (default: derived from cwd)")
		grantsDir    = flag.String("grants-dir", "", "path to deployment/aws-filebased-config/cdk/constructs/internal/grants (default: derived from cwd)")
		verbose      = flag.Bool("v", false, "print skipped (non-AWS) kinds")
	)
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "registrychk: getwd: %v\n", err)
		os.Exit(2)
	}
	if *bridgecfgDir == "" {
		*bridgecfgDir = filepath.Join(cwd, "deployment", "aws-filebased-config", "cdk", "bridgecfg")
	}
	if *grantsDir == "" {
		*grantsDir = filepath.Join(cwd, "deployment", "aws-filebased-config", "cdk", "constructs", "internal", "grants")
	}

	registered, err := buildRegisteredKinds()
	if err != nil {
		fmt.Fprintf(os.Stderr, "registrychk: build registry: %v\n", err)
		os.Exit(2)
	}
	canonical, skipped := classify(registered)

	if *verbose {
		for _, k := range skipped {
			fmt.Fprintf(os.Stderr, "registrychk: skip non-AWS kind %q\n", k)
		}
	}

	prefixes, err := collectBuilderPrefixes(*bridgecfgDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "registrychk: scan bridgecfg: %v\n", err)
		os.Exit(2)
	}

	missing := checkCoverage(canonical, awsDeployableKinds, prefixes, *grantsDir)
	if len(missing) == 0 {
		return
	}
	for _, m := range missing {
		fmt.Fprint(os.Stderr, m)
	}
	os.Exit(1)
}

// buildRegisteredKinds constructs the local *ports.Registry by
// invoking the exported Register function for each adapter the CDK
// supports, then returns the sorted list of registered kinds.
// Adding a new CDK-deployable adapter means extending this set.
func buildRegisteredKinds() ([]string, error) {
	reg := ports.NewRegistry()
	if err := errors.Join(
		awsstore.Register(reg),
		sqsadapter.Register(reg),
		paho.Register(reg),
		nativestore.Register(reg),
	); err != nil {
		return nil, err
	}
	return reg.Kinds(), nil
}

// classify partitions the registered kinds into:
//   - canonical: AWS-deployable kinds (post alias-collapse, deduped, sorted).
//   - skipped:   non-AWS kinds (sorted).
//
// Unknown kinds (registered, AWS-flavoured, but not in the curated
// awsDeployableKinds map) are returned in canonical and surface a
// "missing builder + grants" error downstream — that is the desired
// behaviour: a new AWS adapter MUST extend the curated map.
func classify(registered []string) (canonical, skipped []string) {
	seenCanon := map[string]bool{}
	seenSkip := map[string]bool{}
	for _, k := range registered {
		if isNonAWS(k) {
			if !seenSkip[k] {
				seenSkip[k] = true
				skipped = append(skipped, k)
			}
			continue
		}
		c := canonicalize(k)
		if !seenCanon[c] {
			seenCanon[c] = true
			canonical = append(canonical, c)
		}
	}
	sort.Strings(canonical)
	sort.Strings(skipped)
	return canonical, skipped
}

// canonicalize collapses an alias kind (e.g. "aws.sqs") onto its
// canonical AWS-deployable name (e.g. "sqs") via aliasMap. Kinds
// without an alias entry are returned unchanged.
func canonicalize(kind string) string {
	if c, ok := aliasMap[kind]; ok {
		return c
	}
	return kind
}

// isNonAWS reports whether kind is non-AWS-deployable (azure.*,
// amqp.*, or one of the bare-name non-AWS kinds).
func isNonAWS(kind string) bool {
	if nonAWSExact[kind] {
		return true
	}
	for _, p := range nonAWSPrefixes {
		if strings.HasPrefix(kind, p) {
			return true
		}
	}
	return false
}

// collectBuilderPrefixes parses every .go file in dir and returns
// the set of exported function and method names that begin with
// "With" — both top-level WithFoo helpers and (b *Builder).WithFoo
// methods are collected. Test files (_test.go) are skipped.
func collectBuilderPrefixes(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %q: %w", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			n := fn.Name.Name
			if strings.HasPrefix(n, "With") {
				out[n] = true
			}
		}
	}
	return out, nil
}

// hasBuilderForPrefix returns true if any name in builders matches
// "With<prefix>*" — i.e. starts with "With" followed by the curated
// per-kind prefix. This is the substring check the bridgecfg
// directory must satisfy for the kind.
func hasBuilderForPrefix(builders map[string]bool, prefix string) bool {
	want := "With" + prefix
	for n := range builders {
		if strings.HasPrefix(n, want) {
			return true
		}
	}
	return false
}

// grantsFileExists returns true when grantsDir/file is a regular
// file. Empty file argument means the kind requires no grants
// helper and the check trivially succeeds.
func grantsFileExists(grantsDir, file string) bool {
	if file == "" {
		return true
	}
	st, err := os.Stat(filepath.Join(grantsDir, file))
	if err != nil {
		return false
	}
	return !st.IsDir()
}

// checkCoverage runs the builder + grants assertions for every
// canonical kind and returns one human-readable error block per
// missing kind. An empty return value means full coverage.
func checkCoverage(canonical []string, specs map[string]kindSpec, builders map[string]bool, grantsDir string) []string {
	var out []string
	for _, k := range canonical {
		spec, known := specs[k]
		if !known {
			out = append(out, fmt.Sprintf(
				"registrychk: missing coverage for kind %q:\n"+
					"  - bridgecfg builder: NOT FOUND (kind not in awsDeployableKinds curated map; add an entry or mark non-AWS)\n"+
					"  - grants helper:     UNKNOWN (kind not in awsDeployableKinds curated map)\n",
				k,
			))
			continue
		}
		hasBuilder := hasBuilderForPrefix(builders, spec.builderPrefix)
		hasGrants := grantsFileExists(grantsDir, spec.grantsFile)
		if hasBuilder && hasGrants {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "registrychk: missing coverage for kind %q:\n", k)
		if hasBuilder {
			fmt.Fprintf(&b, "  - bridgecfg builder: OK (found With%s*)\n", spec.builderPrefix)
		} else {
			fmt.Fprintf(&b, "  - bridgecfg builder: NOT FOUND (expected With%s* in cdk/bridgecfg/)\n", spec.builderPrefix)
		}
		if hasGrants {
			if spec.grantsFile == "" {
				fmt.Fprintf(&b, "  - grants helper:     OK (no IAM surface)\n")
			} else {
				fmt.Fprintf(&b, "  - grants helper:     OK (found %s)\n", spec.grantsFile)
			}
		} else {
			fmt.Fprintf(&b, "  - grants helper:     NOT FOUND (expected cdk/constructs/internal/grants/%s)\n", spec.grantsFile)
		}
		out = append(out, b.String())
	}
	return out
}
