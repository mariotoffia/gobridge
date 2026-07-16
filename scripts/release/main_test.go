package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStableVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "stable v0", version: "v0.3.0"},
		{name: "stable v1", version: "v1.2.3"},
		{name: "prerelease", version: "v1.2.3-rc.1", wantErr: true},
		{name: "build metadata", version: "v1.2.3+build.1", wantErr: true},
		{name: "leading zero", version: "v1.02.3", wantErr: true},
		{name: "missing prefix", version: "1.2.3", wantErr: true},
		{name: "missing patch", version: "v1.2", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateStableVersion(tt.version)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateStableVersion(%q) error = %v, wantErr %v", tt.version, err, tt.wantErr)
			}
		})
	}
}

func TestReleaseManifest_ModuleForTag(t *testing.T) {
	t.Parallel()

	manifest := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/mqtt/transport/paho", Layer: 1},
			{Path: "cmd/gobridge", Layer: 2},
		},
		Bootstrap: []string{"testutil/wait"},
	}

	tests := []struct {
		name        string
		tag         string
		wantPath    string
		wantVersion string
		wantErr     bool
	}{
		{name: "root", tag: "v0.3.0", wantPath: ".", wantVersion: "v0.3.0"},
		{
			name:        "nested",
			tag:         "adapters/mqtt/transport/paho/v0.3.0",
			wantPath:    "adapters/mqtt/transport/paho",
			wantVersion: "v0.3.0",
		},
		{name: "internal helper", tag: "testutil/wait/v0.3.0", wantErr: true},
		{name: "unknown module", tag: "deployment/example/v0.3.0", wantErr: true},
		{name: "prerelease", tag: "cmd/gobridge/v0.3.0-rc.1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotModule, gotVersion, err := manifest.moduleForTag(tt.tag)
			if (err != nil) != tt.wantErr {
				t.Fatalf("moduleForTag(%q) error = %v, wantErr %v", tt.tag, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotModule.Path != tt.wantPath || gotVersion != tt.wantVersion {
				t.Fatalf(
					"moduleForTag(%q) = (%q, %q), want (%q, %q)",
					tt.tag,
					gotModule.Path,
					gotVersion,
					tt.wantPath,
					tt.wantVersion,
				)
			}
		})
	}
}

func TestInspectModule_FindsReleaseBlockingManifestEntries(t *testing.T) {
	t.Parallel()

	manifest := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/example", Layer: 1},
		},
		Bootstrap: []string{"testutil/wait"},
	}
	module := moduleManifest{
		Path: "adapters/example",
		Requires: []moduleRequirement{
			{Path: "github.com/mariotoffia/gobridge", Version: "v0.0.0"},
			{
				Path:    "github.com/mariotoffia/gobridge/testutil/wait",
				Version: "v0.0.0-00010101000000-000000000000",
			},
			{
				Path:    "github.com/mariotoffia/gobridge/testutil/wait",
				Version: "v0.0.0-20260716010101-not-a-revision",
			},
		},
		Replaces: []moduleReplacement{
			{
				OldPath: "github.com/mariotoffia/gobridge",
				NewPath: "../..",
			},
		},
	}

	violations, err := inspectModule(manifest, module, "")
	if err != nil {
		t.Fatalf("inspectModule() error = %v", err)
	}

	gotKinds := make([]violationKind, 0, len(violations))
	for _, violation := range violations {
		gotKinds = append(gotKinds, violation.Kind)
	}
	wantKinds := []violationKind{
		violationExactZero,
		violationAllZeroPseudo,
		violationMalformedPseudo,
		violationLocalReplace,
	}
	for _, want := range wantKinds {
		if !slices.Contains(gotKinds, want) {
			t.Errorf("inspectModule() kinds = %v, missing %q", gotKinds, want)
		}
	}
}

func TestInspectModule_RejectsUndeclaredAndNonLowerDependencies(t *testing.T) {
	t.Parallel()

	manifest := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/first", Layer: 1},
			{Path: "adapters/second", Layer: 1},
		},
		Bootstrap: []string{"testutil/wait"},
	}

	tests := []struct {
		name       string
		dependency string
	}{
		{name: "same layer", dependency: "github.com/mariotoffia/gobridge/adapters/second"},
		{name: "undeclared sibling", dependency: "github.com/mariotoffia/gobridge/testutil/other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := inspectModule(manifest, moduleManifest{
				Path: "adapters/first",
				Requires: []moduleRequirement{
					{Path: tt.dependency, Version: "v0.3.0"},
				},
			}, "v0.3.0")
			if err == nil {
				t.Fatalf("inspectModule() error = nil, want dependency error for %q", tt.dependency)
			}
		})
	}
}

func TestRunModuleChecks_DisablesWorkspaceAndUsesUncachedTests(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	moduleDir := filepath.FromSlash("/repo/adapters/example")

	if err := runModuleChecks(context.Background(), runner, moduleDir); err != nil {
		t.Fatalf("runModuleChecks() error = %v", err)
	}

	wantArgs := [][]string{
		{"mod", "download"},
		{"mod", "verify"},
		{"build", "./..."},
		{"test", "-count=1", "./..."},
	}
	if len(runner.requests) != len(wantArgs) {
		t.Fatalf("runModuleChecks() commands = %d, want %d", len(runner.requests), len(wantArgs))
	}
	for i, request := range runner.requests {
		if request.Name != "go" || !slices.Equal(request.Args, wantArgs[i]) {
			t.Errorf("command %d = %s %v, want go %v", i, request.Name, request.Args, wantArgs[i])
		}
		if request.Dir != moduleDir {
			t.Errorf("command %d dir = %q, want %q", i, request.Dir, moduleDir)
		}
		if request.Env["GOWORK"] != "off" {
			t.Errorf("command %d GOWORK = %q, want off", i, request.Env["GOWORK"])
		}
		if request.Timeout <= 0 {
			t.Errorf("command %d timeout = %s, want bounded deadline", i, request.Timeout)
		}
	}
}

func TestStageModuleManifest_RewritesDeclaredDependenciesOnly(t *testing.T) {
	t.Parallel()

	manifest := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/example", Layer: 1},
		},
		Bootstrap: []string{"testutil/wait"},
	}
	input := []byte(`module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
)

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)
`)

	got, err := stageModuleManifest(
		manifest,
		"adapters/example/go.mod",
		input,
		"v0.3.0",
		map[string]string{"testutil/wait": "v0.0.0-20260716010101-0123456789ab"},
	)
	if err != nil {
		t.Fatalf("stageModuleManifest() error = %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"github.com/mariotoffia/gobridge v0.3.0",
		"github.com/mariotoffia/gobridge/testutil/wait v0.0.0-20260716010101-0123456789ab",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stageModuleManifest() output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "replace") {
		t.Errorf("stageModuleManifest() retained local replace:\n%s", text)
	}
}

func TestDeriveBootstrapVersions_UsesGoListCommitQuery(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	repo := t.TempDir()
	helperMod := filepath.Join(repo, "wait.mod")
	writeTestFile(t, helperMod, `module github.com/mariotoffia/gobridge/testutil/wait

go 1.25.0
`)
	listed := listedModule{
		Path:    "github.com/mariotoffia/gobridge/testutil/wait",
		Version: "v0.0.0-20260716010101-0123456789ab",
		GoMod:   helperMod,
	}
	listed.Origin.Hash = commit
	result, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("Marshal(listed) error = %v", err)
	}
	runner := &recordingRunner{
		outputs: [][]byte{result},
	}
	manifest := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Bootstrap:    []string{"testutil/wait"},
	}

	versions, err := deriveBootstrapVersions(
		context.Background(),
		runner,
		manifest,
		repo,
		commit,
		"v0.3.0",
	)
	if err != nil {
		t.Fatalf("deriveBootstrapVersions() error = %v", err)
	}
	if got := versions["testutil/wait"]; got != "v0.0.0-20260716010101-0123456789ab" {
		t.Fatalf("derived wait version = %q", got)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("go list commands = %d, want 1", len(runner.requests))
	}
	request := runner.requests[0]
	wantQuery := "github.com/mariotoffia/gobridge/testutil/wait@" + commit
	if request.Name != "go" || !slices.Equal(request.Args, []string{"list", "-m", "-json", wantQuery}) {
		t.Fatalf("derive command = %s %v, want go list -m -json %s", request.Name, request.Args, wantQuery)
	}
}

func TestValidateSmokeTag_RequiresFinalModule(t *testing.T) {
	t.Parallel()

	manifest := releaseManifest{
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "cmd/gobridge", Layer: 1},
		},
	}

	if _, err := validateSmokeTag(manifest, "v0.3.0"); err == nil {
		t.Fatal("validateSmokeTag(root) error = nil, want final-module error")
	}
	version, err := validateSmokeTag(manifest, "cmd/gobridge/v0.3.0")
	if err != nil {
		t.Fatalf("validateSmokeTag(cmd) error = %v", err)
	}
	if version != "v0.3.0" {
		t.Fatalf("validateSmokeTag(cmd) = %q, want v0.3.0", version)
	}
}

type recordingRunner struct {
	requests []commandRequest
	outputs  [][]byte
	err      error
}

func (r *recordingRunner) run(_ context.Context, request commandRequest) ([]byte, error) {
	r.requests = append(r.requests, request)
	if r.err != nil {
		return nil, r.err
	}
	if len(r.outputs) == 0 {
		return nil, nil
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func TestRunModuleChecks_PropagatesCommandFailure(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{err: errors.New("command failed")}
	if err := runModuleChecks(context.Background(), runner, "/repo"); err == nil {
		t.Fatal("runModuleChecks() error = nil, want command failure")
	}
}

func TestStrictAll_RejectsForbiddenManifestBeforePublicCommands(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	manifest := releaseManifest{
		Schema:       1,
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/example", Layer: 1},
			{Path: "httpapi", Layer: 2},
			{Path: "cmd/gobridge", Layer: 3},
		},
	}
	files := map[string]string{
		"go.mod": `module github.com/mariotoffia/gobridge

go 1.25.0
`,
		"adapters/example/go.mod": `module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0
replace github.com/mariotoffia/gobridge => ../..
`,
		"httpapi/go.mod": `module github.com/mariotoffia/gobridge/httpapi

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`,
		"cmd/gobridge/go.mod": `module github.com/mariotoffia/gobridge/cmd/gobridge

go 1.25.0

require github.com/mariotoffia/gobridge/httpapi v0.3.0
`,
	}
	if err := os.MkdirAll(filepath.Join(repo, "processors"), 0o755); err != nil {
		t.Fatalf("MkdirAll(processors) error = %v", err)
	}
	for name, content := range files {
		filename := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filename, err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", filename, err)
		}
	}

	runner := &recordingRunner{}
	err := strictAll(context.Background(), runner, repo, manifest, "v0.3.0")
	if err == nil {
		t.Fatal("strictAll() error = nil, want forbidden-manifest rejection")
	}
	for _, want := range []string{"exact-v0.0.0", "local-replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("strictAll() error = %q, want %q", err, want)
		}
	}
	if len(runner.requests) != 0 {
		t.Fatalf("strictAll() ran %d public commands before static rejection", len(runner.requests))
	}
}
