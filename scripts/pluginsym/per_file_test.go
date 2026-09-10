package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginsymCLI runs the real command in a child test process.
func TestPluginsymCLI(t *testing.T) {
	dir := os.Getenv("PLUGINSYM_TEST_DIR")
	if dir == "" {
		return
	}
	os.Args = []string{os.Args[0], "-dir", dir, "-v"}
	flag.CommandLine = flag.NewFlagSet("pluginsym", flag.ExitOnError)
	main()
	os.Exit(0)
}

func checkDirectory(t *testing.T, dir, want string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPluginsymCLI$")
	cmd.Env = append(os.Environ(), "PLUGINSYM_TEST_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if want == "" {
		if err != nil {
			t.Fatalf("pluginsym: %v\n%s", err, out)
		}
		return
	}
	if err == nil || !strings.Contains(string(out), want) {
		t.Fatalf("pluginsym error = %v, output = %s; want %q", err, out, want)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("violations must exit 1, got %v", err)
	}
}

// TestPerFileSymmetry_FamilyOK verifies symmetric tagged wiring beside an empty main.
func TestPerFileSymmetry_FamilyOK(t *testing.T) {
	checkDirectory(t, "testdata/family_ok", "")
}

// TestPerFileSymmetry_FamilySeedOK verifies Supervisor and Builder kinds deduplicate.
func TestPerFileSymmetry_FamilySeedOK(t *testing.T) {
	checkDirectory(t, "testdata/family_seed_ok", "")
}

// TestPerFileSymmetry_FamilyAsymmetric rejects a decoder without its factory.
func TestPerFileSymmetry_FamilyAsymmetric(t *testing.T) {
	checkDirectory(t, "testdata/family_asymmetric", "R1")
}

// TestPerFileSymmetry_StubWithCalls rejects wiring in a negated family file.
func TestPerFileSymmetry_StubWithCalls(t *testing.T) {
	checkDirectory(t, "testdata/stub_with_calls", "R3")
}

// TestPerFileSymmetry_DuplicateAdapter rejects registration in two separate files.
func TestPerFileSymmetry_DuplicateAdapter(t *testing.T) {
	checkDirectory(t, "testdata/duplicate_adapter", "R2")
}

// TestPerFileSymmetry_BadConstraint rejects a platform-dependent family.
func TestPerFileSymmetry_BadConstraint(t *testing.T) {
	checkDirectory(t, "testdata/bad_constraint", "R4")
}

// TestPerFileSymmetry_UntaggedWiring rejects seed factories in an untagged file.
func TestPerFileSymmetry_UntaggedWiring(t *testing.T) {
	checkDirectory(t, "testdata/untagged_wiring", "R5")
}

// TestPerFileSymmetry_Constraints verifies only exact additive families and inverse stubs pass.
func TestPerFileSymmetry_Constraints(t *testing.T) {
	for _, tc := range []struct {
		name, expr, want string
	}{
		{"family", "gobridge_mqtt || gobridge_all", ""},
		{"parentheses", "(gobridge_mqtt || gobridge_all)", ""},
		{"stub", "!gobridge_mqtt && !gobridge_all", ""},
		{"unrelated", "!linux", ""},
		{"all_only", "gobridge_all", "R4"},
		{"family_only", "gobridge_mqtt", "R4"},
		{"negated_only", "!gobridge_mqtt", "R4"},
		{"unsafe_stub", "!gobridge_mqtt || !gobridge_all", "R4"},
		{"mixed", "!gobridge_native && gobridge_mqtt", "R4"},
		{"extra", "(gobridge_mqtt || gobridge_all) && linux", "R4"},
		{"double_negation", "!!gobridge_mqtt || gobridge_all", "R4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "plugins.go"), "//go:build "+tc.expr+"\n\npackage main\n")
			checkDirectory(t, dir, tc.want)
		})
	}
}

// TestPerFileSymmetry_FilenameConstraint rejects implicit GOOS/GOARCH restrictions.
func TestPerFileSymmetry_FilenameConstraint(t *testing.T) {
	for _, name := range []string{"plugins_linux.go", "plugins_arm64.go", "plugins_windows_amd64.go"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, name), "//go:build gobridge_mqtt || gobridge_all\n\npackage main\n")
			checkDirectory(t, dir, "R4")
		})
	}
}

// TestPerFileSymmetry_IgnoresTests verifies test registrations cannot affect the gate.
func TestPerFileSymmetry_IgnoresTests(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "main_test.go"), "package main\nfunc test() { b.RegisterStoreFactory(\"memory\", nil) }\n")
	checkDirectory(t, dir, "")
}

// TestPerFileSymmetry_DynamicWiring rejects calls whose kind is not statically visible.
func TestPerFileSymmetry_DynamicWiring(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plugins.go"), "//go:build gobridge_native || gobridge_all\n\npackage main\nfunc wire() { b.RegisterStoreFactory(kind, nil) }\n")
	checkDirectory(t, dir, "string literal")
}

// TestPerFileSymmetry_UnrelatedNegation rejects wiring even behind a non-family constraint.
func TestPerFileSymmetry_UnrelatedNegation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plugins.go"), "//go:build !linux\n\npackage main\nfunc wire() { b.RegisterStoreFactory(\"memory\", nil) }\n")
	checkDirectory(t, dir, "R5")
}

// TestPerFileSymmetry_IndirectCalls verifies method values cannot hide registrations.
func TestPerFileSymmetry_IndirectCalls(t *testing.T) {
	for _, expr := range []string{"", "//go:build !gobridge_mqtt && !gobridge_all\n\n"} {
		for _, body := range []string{
			"register := paho.Register; _ = register(reg)",
			"wire := sup.RegisterTransport; wire(\"mqtt\", nil)",
			"callbacks := []any{paho.Register, sup.RegisterTransport}; _ = callbacks",
		} {
			t.Run(expr+body, func(t *testing.T) {
				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "plugins.go"), expr+`package main
import paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
func plugins() { `+body+" }\n")
				checkDirectory(t, dir, "must be called directly")
			})
		}
	}
}
