package main

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestACLRules proves both ACL rules on a fixture adapter package:
//   - Import confinement: a non-ACL file (leak.go) importing the vendor
//     SDK is flagged.
//   - Export confinement: an ACL file (acl_widget.go) that re-exports an
//     SDK type under an exported alias/defined type/slice is flagged,
//     while an //aclcheck:allow-export annotation, an unexported alias,
//     and the wrap-in-unexported-field pattern all pass.
func TestACLRules(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "example/adapters/widget")
}
