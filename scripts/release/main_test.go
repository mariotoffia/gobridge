package main

import (
	"context"
	"errors"
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
			{Path: "testutil/example", Layer: 1},
			{Path: "adapters/mqtt/transport/paho", Layer: 2},
			{Path: "cmd/gobridge", Layer: 3},
		},
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
		{
			name:        "test helper",
			tag:         "testutil/example/v0.3.0",
			wantPath:    "testutil/example",
			wantVersion: "v0.3.0",
		},
		{name: "undeclared helper", tag: "testutil/wait/v0.3.0", wantErr: true},
		{name: "internal tests module", tag: "tests/integration/v0.3.0", wantErr: true},
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
			{Path: "testutil/example", Layer: 1},
			{Path: "adapters/example", Layer: 2},
		},
	}
	module := moduleManifest{
		Path: "adapters/example",
		Requires: []moduleRequirement{
			{Path: "github.com/mariotoffia/gobridge", Version: "v0.0.0"},
			{
				Path:    "github.com/mariotoffia/gobridge/testutil/example",
				Version: "v0.0.0-00010101000000-000000000000",
			},
			{
				Path:    "github.com/mariotoffia/gobridge/testutil/example",
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

func TestInspectModule_RejectsRemoteReplaceAndExclude(t *testing.T) {
	t.Parallel()

	moduleFile, err := parseModuleManifest("go.mod", []byte(`module github.com/mariotoffia/gobridge/adapters/example

go 1.25

require example.com/dependency v1.2.3

replace example.com/dependency v1.2.3 => example.com/fork v1.2.4

exclude example.com/dependency v1.2.2
`))
	if err != nil {
		t.Fatalf("parseModuleManifest() error = %v", err)
	}
	moduleFile.Path = "adapters/example"

	violations, err := inspectModule(releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/example", Layer: 1},
		},
	}, moduleFile, "v1.2.3")
	if err != nil {
		t.Fatalf("inspectModule() error = %v", err)
	}

	kinds := make([]violationKind, 0, len(violations))
	for _, violation := range violations {
		kinds = append(kinds, violation.Kind)
	}
	for _, want := range []violationKind{"replace-directive", "exclude-directive"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("inspectModule() kinds = %v, missing %q", kinds, want)
		}
	}
}

func TestInspectModule_RejectsUndeclaredAndNonLowerDependencies(t *testing.T) {
	t.Parallel()

	adapters := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/first", Layer: 1},
			{Path: "adapters/second", Layer: 1},
		},
	}
	helpers := releaseManifest{
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "testutil/example", Layer: 1},
			{Path: "adapters/example", Layer: 2},
		},
	}

	tests := []struct {
		name       string
		manifest   releaseManifest
		modulePath string
		dependency string
		wantDetail string
	}{
		{
			name:       "same layer",
			manifest:   adapters,
			modulePath: "adapters/first",
			dependency: "github.com/mariotoffia/gobridge/adapters/second",
			wantDetail: "lower layer",
		},
		{
			name:       "undeclared sibling",
			manifest:   adapters,
			modulePath: "adapters/first",
			dependency: "github.com/mariotoffia/gobridge/testutil/other",
			wantDetail: "undeclared repository sibling",
		},
		{
			name:       "helper requires an adapter above it",
			manifest:   helpers,
			modulePath: "testutil/example",
			dependency: "github.com/mariotoffia/gobridge/adapters/example",
			wantDetail: "lower layer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := inspectModule(tt.manifest, moduleManifest{
				Path: tt.modulePath,
				Requires: []moduleRequirement{
					{Path: tt.dependency, Version: "v0.3.0"},
				},
			}, "v0.3.0")
			if err == nil || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("inspectModule() error = %v, want %q for %q", err, tt.wantDetail, tt.dependency)
			}
		})
	}
}

func TestRunModuleChecks_DisablesWorkspaceAndProvesConsumability(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	moduleDir := filepath.FromSlash("/repo/adapters/example")

	if err := runModuleChecks(context.Background(), runner, moduleDir); err != nil {
		t.Fatalf("runModuleChecks() error = %v", err)
	}

	// Fetch the whole graph, check its checksums, compile the replace-free
	// source against those exact versions. No test step: the tag's commit
	// differs from main only in go.mod/go.sum, so CI has already run the tests
	// on identical source and a consumer never compiles them.
	wantArgs := [][]string{
		{"mod", "download"},
		{"mod", "verify"},
		{"build", "./..."},
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
			{Path: "testutil/example", Layer: 1},
			{Path: "adapters/example", Layer: 2},
		},
	}
	input := []byte(`module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/example v0.0.0
	github.com/stretchr/testify v1.11.1
)

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/testutil/example => ../../testutil/example
)
`)

	got, err := stageModuleManifest(
		manifest,
		"adapters/example/go.mod",
		input,
		"v0.3.0",
	)
	if err != nil {
		t.Fatalf("stageModuleManifest() error = %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"github.com/mariotoffia/gobridge v0.3.0",
		"github.com/mariotoffia/gobridge/testutil/example v0.3.0",
		"github.com/stretchr/testify v1.11.1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("stageModuleManifest() output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "replace") {
		t.Errorf("stageModuleManifest() retained local replace:\n%s", text)
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

	repo, manifest := writeFixtureRepository(t, true)
	writeTestFile(t, filepath.Join(repo, "adapters", "example", "go.mod"), `module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0
replace github.com/mariotoffia/gobridge => ../..
`)

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
