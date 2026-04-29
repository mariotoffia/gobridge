// aclcheck implements a go/analysis pass that enforces the
// hexagonal anti-corruption layer naming convention.
//
// Inside an adapter package directory (any path containing "/adapters/"),
// the only files allowed to import vendor SDK packages are:
//
//   - Files whose name matches acl*.go (e.g., acl_envelope.go).
//   - Files in an acl/ sub-directory of the adapter package.
//
// The analyzer is configured with a list of "vendor SDK" import-path
// prefixes. The default set covers the SDKs this project's adapters
// currently use; extend `vendorPatterns` when adding a new adapter
// technology.
//
// Why this rule: the hexagonal anti-corruption layer is the seam
// between an external SDK's vocabulary (its types, errors, encodings)
// and the project's domain types. Concentrating that seam in
// explicitly-named files makes it visible to reviewers — a future
// maintainer can find the SDK boundary at a glance and avoids
// silently leaking SDK types across the adapter.
package main

import (
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

// Analyzer is the singleton aclcheck analysis pass.
var Analyzer = &analysis.Analyzer{
	Name: "aclcheck",
	Doc:  "checks that vendor SDK imports stay confined to ACL files in adapter packages",
	Run:  run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	// Only check adapter packages.
	if !strings.Contains(pass.Pkg.Path(), "/adapters/") {
		return nil, nil //nolint:nilnil // analysis.Run convention: nil result + nil err = no findings.
	}
	for _, f := range pass.Files {
		filename := pass.Fset.Position(f.Pos()).Filename
		if isExempt(filename) {
			continue
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !isVendorSDK(path) {
				continue
			}
			pass.Reportf(imp.Pos(),
				"vendor SDK import %q is allowed only in ACL files (acl_*.go) or acl/ directories; move the SDK boundary into an ACL", path)
		}
	}
	return nil, nil //nolint:nilnil
}

// isExempt returns true for filenames the analyzer should ignore.
//
// Test files are exempt: tests legitimately construct SDK types
// directly (mocks, fixtures, edge-case probes) and forcing the
// pattern there would push test code into a fictional ACL.
//
// ACL files (acl_*.go or under acl/) are exempt by definition.
func isExempt(filename string) bool {
	base := filepath.Base(filename)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	if strings.HasPrefix(base, "acl") {
		return true
	}
	dir := filepath.Base(filepath.Dir(filename))
	return dir == "acl"
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
