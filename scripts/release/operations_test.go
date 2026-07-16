package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testReleaseVersion = "v0.3.0"

func TestReleaseManifest_Validate(t *testing.T) {
	t.Parallel()

	valid := fixtureManifest()
	tests := []struct {
		name   string
		mutate func(*releaseManifest)
	}{
		{
			name: "internal module published",
			mutate: func(manifest *releaseManifest) {
				manifest.Published[1].Path = "deployment/example"
			},
		},
		{
			name: "missing root",
			mutate: func(manifest *releaseManifest) {
				manifest.Published = manifest.Published[1:]
			},
		},
		{
			name: "layer gap",
			mutate: func(manifest *releaseManifest) {
				manifest.Published[1].Layer = 2
			},
		},
		{
			name: "final module not final",
			mutate: func(manifest *releaseManifest) {
				manifest.Published[len(manifest.Published)-1].Layer = 2
			},
		},
		{
			name: "bootstrap outside testutil",
			mutate: func(manifest *releaseManifest) {
				manifest.Bootstrap = []string{"tests/helper"}
			},
		},
	}

	if err := valid.validate(); err != nil {
		t.Fatalf("valid manifest error = %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manifest := fixtureManifest()
			tt.mutate(&manifest)
			if err := manifest.validate(); err == nil {
				t.Fatal("validate() error = nil, want invalid manifest error")
			}
		})
	}
}

func TestRunSourcePreflight_ReportsStructureAndInventory(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, true)
	adapterMod := filepath.Join(repo, "adapters", "example", "go.mod")
	writeTestFile(t, adapterMod, `module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0
replace github.com/mariotoffia/gobridge => ../..
`)

	var output bytes.Buffer
	if err := runSourcePreflight(repo, manifest, &output); err != nil {
		t.Fatalf("runSourcePreflight() error = %v", err)
	}
	for _, want := range []string{
		"Release source preflight PASS.",
		"Published modules: 4; layer 0=1; layer 1=1; layer 2=1; layer 3=1",
		"exact-v0.0.0: 1",
		"local-replace: 1",
		"strict release gates reject it",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("source output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunCLI_ListUsesCanonicalManifest(t *testing.T) {
	t.Parallel()

	repo, _ := writeFixtureRepository(t, true)
	var output bytes.Buffer
	if err := runCLI(
		context.Background(),
		[]string{"list", "--repo", repo, "--layer", "1", "--format", "tag", "--version", testReleaseVersion},
		&output,
		&recordingRunner{},
	); err != nil {
		t.Fatalf("runCLI(list) error = %v", err)
	}
	if got, want := output.String(), "adapters/example/v0.3.0\n"; got != want {
		t.Fatalf("runCLI(list) output = %q, want %q", got, want)
	}
}

func TestRunCLI_VerificationAndStagingCommands(t *testing.T) {
	tests := []struct {
		name       string
		args       func(string) []string
		prepare    func(*testing.T, string, *releaseManifest)
		wantOutput string
	}{
		{
			name: "source",
			args: func(repo string) []string {
				return []string{"source", "--repo", repo}
			},
			wantOutput: "Release source preflight PASS.",
		},
		{
			name: "strict all",
			args: func(repo string) []string {
				return []string{"strict-all", "--repo", repo, "--version", testReleaseVersion}
			},
			wantOutput: "Strict published-module gate PASS",
		},
		{
			name: "strict tag",
			args: func(repo string) []string {
				return []string{
					"strict-tag",
					"--repo",
					repo,
					"--tag",
					"adapters/example/v0.3.0",
				}
			},
			wantOutput: "path=adapters/example",
		},
		{
			name: "strict module",
			args: func(repo string) []string {
				return []string{
					"strict-module",
					"--repo",
					repo,
					"--module",
					"adapters/example",
					"--version",
					testReleaseVersion,
				}
			},
			wantOutput: "Strict pre-tag gate PASS",
		},
		{
			name: "stage module",
			args: func(repo string) []string {
				return []string{
					"stage-module",
					"--repo",
					repo,
					"--module",
					"adapters/example",
					"--version",
					testReleaseVersion,
				}
			},
			prepare: func(t *testing.T, repo string, _ *releaseManifest) {
				t.Helper()
				writeTestFile(t, filepath.Join(repo, "adapters", "example", "go.mod"), `module github.com/mariotoffia/gobridge/adapters/example

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.0.0
	replace github.com/mariotoffia/gobridge => ../..
	`)
			},
			wantOutput: "Staged and strictly verified adapters/example",
		},
		{
			name: "stage bootstrap",
			args: func(repo string) []string {
				return []string{
					"stage-bootstrap",
					"--repo",
					repo,
					"--version",
					testReleaseVersion,
				}
			},
			prepare: func(t *testing.T, repo string, manifest *releaseManifest) {
				t.Helper()
				manifest.Bootstrap = []string{"testutil/wait"}
				writeManifestFile(t, repo, *manifest)
				writeTestFile(t, filepath.Join(repo, "testutil", "wait", "go.mod"), `module github.com/mariotoffia/gobridge/testutil/wait

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.0.0
	replace github.com/mariotoffia/gobridge => ../..
	`)
			},
			wantOutput: "Staged bootstrap manifests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, manifest := writeFixtureRepository(t, true)
			if tt.prepare != nil {
				tt.prepare(t, repo, &manifest)
			}
			var output bytes.Buffer
			if err := runCLI(
				context.Background(),
				tt.args(repo),
				&output,
				newSuccessfulReleaseRunner(t),
			); err != nil {
				t.Fatalf("runCLI() error = %v", err)
			}
			if !strings.Contains(output.String(), tt.wantOutput) {
				t.Fatalf("runCLI() output = %q, want substring %q", output.String(), tt.wantOutput)
			}
		})
	}
}

func TestRunCLI_DeriveBootstrap(t *testing.T) {
	t.Parallel()

	const (
		commit = "0123456789abcdef0123456789abcdef01234567"
		pseudo = "v0.0.0-20260716010101-0123456789ab"
	)
	repo, manifest := writeFixtureRepository(t, true)
	manifest.Bootstrap = []string{"testutil/wait"}
	writeManifestFile(t, repo, manifest)
	helperMod := filepath.Join(repo, "downloaded-wait.mod")
	writeTestFile(t, helperMod, `module github.com/mariotoffia/gobridge/testutil/wait

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`)
	listed := listedModule{
		Path:    "github.com/mariotoffia/gobridge/testutil/wait",
		Version: pseudo,
		GoMod:   helperMod,
	}
	listed.Origin.Hash = commit
	result, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("Marshal(listed) error = %v", err)
	}
	runner := &recordingRunner{outputs: [][]byte{result}}

	var output bytes.Buffer
	err = runCLI(
		context.Background(),
		[]string{
			"derive-bootstrap",
			"--repo",
			repo,
			"--commit",
			commit,
			"--version",
			testReleaseVersion,
		},
		&output,
		runner,
	)
	if err != nil {
		t.Fatalf("runCLI(derive-bootstrap) error = %v", err)
	}
	if !strings.Contains(output.String(), "testutil/wait\t"+pseudo) {
		t.Fatalf("derive output = %q", output.String())
	}
}

func TestRunCLI_Smoke(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, true)
	manifest.Published[1].Path = "adapters/mqtt/transport/paho"
	oldDir := filepath.Join(repo, "adapters", "example")
	newDir := filepath.Join(repo, "adapters", "mqtt", "transport", "paho")
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", newDir, err)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("Rename(%s, %s) error = %v", oldDir, newDir, err)
	}
	writeTestFile(t, filepath.Join(newDir, "go.mod"), `module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`)
	writeManifestFile(t, repo, manifest)

	runner := newSuccessfulReleaseRunner(t)
	runner.sideEffect = func(request commandRequest) error {
		if request.Name == "go" && slices.Equal(request.Args, []string{
			"mod",
			"init",
			"example.com/gobridge-release-smoke",
		}) {
			writeTestFile(t, filepath.Join(request.Dir, "go.mod"), `module example.com/gobridge-release-smoke

go 1.25.0
`)
		}
		return nil
	}

	var output bytes.Buffer
	if err := runCLI(
		context.Background(),
		[]string{
			"smoke",
			"--repo",
			repo,
			"--tag",
			"cmd/gobridge/v0.3.0",
		},
		&output,
		runner,
	); err != nil {
		t.Fatalf("runCLI(smoke) error = %v", err)
	}
	if !strings.Contains(output.String(), "External consumer smoke PASS") {
		t.Fatalf("smoke output = %q", output.String())
	}
}

func TestRunCLI_RejectsMissingAndUnknownCommands(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"strict-all"},
		{"strict-all", "--version"},
	} {
		if err := runCLI(
			context.Background(),
			args,
			&bytes.Buffer{},
			&recordingRunner{},
		); err == nil {
			t.Errorf("runCLI(%v) error = nil", args)
		}
	}
}

func TestListModules_AllFormats(t *testing.T) {
	t.Parallel()

	manifest := fixtureManifest()
	tests := []struct {
		format  string
		version string
		want    string
	}{
		{format: "path", want: "adapters/example"},
		{format: "import", want: "github.com/mariotoffia/gobridge/adapters/example"},
		{format: "tag", version: testReleaseVersion, want: "adapters/example/v0.3.0"},
		{format: "tsv", want: "1\tadapters/example\tgithub.com/mariotoffia/gobridge/adapters/example"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			if err := listModules(manifest, 1, tt.format, tt.version, &output); err != nil {
				t.Fatalf("listModules() error = %v", err)
			}
			if strings.TrimSpace(output.String()) != tt.want {
				t.Fatalf("listModules() = %q, want %q", strings.TrimSpace(output.String()), tt.want)
			}
		})
	}
}

func TestMergeEnvironment_OverridesAndSorts(t *testing.T) {
	t.Parallel()

	got := mergeEnvironment(
		[]string{"Z=last", "A=old"},
		map[string]string{"A": "new", "M": "middle"},
	)
	want := []string{"A=new", "M=middle", "Z=last"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeEnvironment() = %v, want %v", got, want)
	}
}

func TestExecRunner_RunsCommandWithEnvironment(t *testing.T) {
	t.Parallel()

	output, err := (execRunner{}).run(context.Background(), commandRequest{
		Dir:  t.TempDir(),
		Env:  map[string]string{"GOWORK": "off"},
		Name: "go",
		Args: []string{"version"},
	})
	if err != nil {
		t.Fatalf("execRunner.run() error = %v", err)
	}
	if !strings.HasPrefix(string(output), "go version ") {
		t.Fatalf("go version output = %q", output)
	}
}

func TestInspectBootstrapModule_ReportsOnlyInternalPreparationDebt(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	manifest.Bootstrap = []string{"testutil/wait"}
	helperMod := filepath.Join(repo, "testutil", "wait", "go.mod")
	writeTestFile(t, helperMod, `module github.com/mariotoffia/gobridge/testutil/wait

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.0.0
	replace github.com/mariotoffia/gobridge => ../..
	`)

	violations, err := inspectBootstrapModule(repo, manifest, "testutil/wait", "")
	if err != nil {
		t.Fatalf("inspectBootstrapModule() error = %v", err)
	}
	kinds := make([]violationKind, 0, len(violations))
	for _, violation := range violations {
		kinds = append(kinds, violation.Kind)
	}
	if !slices.Contains(kinds, violationExactZero) || !slices.Contains(kinds, violationLocalReplace) {
		t.Fatalf("bootstrap violation kinds = %v", kinds)
	}
}

func TestInspectRepository_TracksDeclaredBootstrapUsage(t *testing.T) {
	t.Parallel()

	const pseudo = "v0.0.0-20260716010101-0123456789ab"
	repo, manifest := writeFixtureRepository(t, false)
	manifest.Bootstrap = []string{"testutil/wait"}
	writeTestFile(t, filepath.Join(repo, "adapters", "example", "go.mod"), `module github.com/mariotoffia/gobridge/adapters/example

	go 1.25.0

	require (
		github.com/mariotoffia/gobridge v0.3.0
		github.com/mariotoffia/gobridge/testutil/wait `+pseudo+`
	)
	`)
	writeTestFile(t, filepath.Join(repo, "testutil", "wait", "go.mod"), `module github.com/mariotoffia/gobridge/testutil/wait

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.3.0
	replace github.com/mariotoffia/gobridge => ../..
	`)

	state, err := inspectRepository(repo, manifest, testReleaseVersion)
	if err != nil {
		t.Fatalf("inspectRepository() error = %v", err)
	}
	if len(state.Violations) != 0 {
		t.Fatalf("published violations = %v", state.Violations)
	}
	if len(state.BootstrapViolations) != 1 ||
		state.BootstrapViolations[0].Kind != violationLocalReplace {
		t.Fatalf("bootstrap violations = %v", state.BootstrapViolations)
	}
}

func TestResolveSiblingRequirements_ValidatesBootstrapOriginAndGoMod(t *testing.T) {
	t.Parallel()

	const (
		commit = "0123456789abcdef0123456789abcdef01234567"
		pseudo = "v0.0.0-20260716010101-0123456789ab"
	)
	repo := t.TempDir()
	helperMod := filepath.Join(repo, "wait.mod")
	writeTestFile(t, helperMod, `module github.com/mariotoffia/gobridge/testutil/wait

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.3.0
	`)
	manifest := fixtureManifest()
	manifest.Bootstrap = []string{"testutil/wait"}
	rootResult, err := json.Marshal(listedModule{
		Path:    manifest.ModulePrefix,
		Version: testReleaseVersion,
	})
	if err != nil {
		t.Fatalf("Marshal(root result) error = %v", err)
	}
	helperResult := listedModule{
		Path:    manifest.importPath("testutil/wait"),
		Version: pseudo,
		GoMod:   helperMod,
	}
	helperResult.Origin.Hash = commit
	helperJSON, err := json.Marshal(helperResult)
	if err != nil {
		t.Fatalf("Marshal(helper result) error = %v", err)
	}
	runner := &recordingRunner{outputs: [][]byte{rootResult, helperJSON}}
	moduleFile := moduleManifest{
		Path: "adapters/example",
		Requires: []moduleRequirement{
			{Path: manifest.ModulePrefix, Version: testReleaseVersion},
			{Path: manifest.importPath("testutil/wait"), Version: pseudo},
		},
	}

	if err := resolveSiblingRequirements(
		context.Background(),
		runner,
		repo,
		manifest,
		moduleFile,
		testReleaseVersion,
	); err != nil {
		t.Fatalf("resolveSiblingRequirements() error = %v", err)
	}
}

func TestValidatePublishedSet_RejectsUnlistedAdapter(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	writeTestFile(t, filepath.Join(repo, "adapters", "extra", "go.mod"), `module github.com/mariotoffia/gobridge/adapters/extra

	go 1.25.0
	`)
	err := validatePublishedSet(repo, manifest)
	if err == nil || !strings.Contains(err.Error(), "adapters/extra") {
		t.Fatalf("validatePublishedSet() error = %v, want unlisted adapter", err)
	}
}

func TestListModules_RejectsInvalidSelection(t *testing.T) {
	t.Parallel()

	manifest := fixtureManifest()
	tests := []struct {
		layer   int
		format  string
		version string
	}{
		{layer: -2, format: "path"},
		{layer: 99, format: "path"},
		{layer: 1, format: "unknown"},
		{layer: 1, format: "tag"},
		{layer: 1, format: "path", version: testReleaseVersion},
	}
	for _, tt := range tests {
		if err := listModules(
			manifest,
			tt.layer,
			tt.format,
			tt.version,
			&bytes.Buffer{},
		); err == nil {
			t.Errorf("listModules(%d, %q, %q) error = nil", tt.layer, tt.format, tt.version)
		}
	}
}

func TestValidateResolvedBootstrapGoMod(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	filename := filepath.Join(repo, "go.mod")
	writeTestFile(t, filename, `module github.com/mariotoffia/gobridge/testutil/wait

	go 1.25.0

	require github.com/mariotoffia/gobridge v0.3.0
	`)
	manifest := fixtureManifest()
	if err := validateResolvedBootstrapGoMod(manifest, filename, testReleaseVersion); err != nil {
		t.Fatalf("validateResolvedBootstrapGoMod() error = %v", err)
	}

	writeTestFile(t, filename, strings.ReplaceAll(
		string(mustReadTestFile(t, filename)),
		"v0.3.0",
		"v0.2.0",
	))
	if err := validateResolvedBootstrapGoMod(manifest, filename, testReleaseVersion); err == nil {
		t.Fatal("validateResolvedBootstrapGoMod() error = nil for wrong root version")
	}
}

func TestStrictModule_ValidTagRunsResolutionAndGoGates(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	runner := newSuccessfulReleaseRunner(t)

	if err := strictModule(
		context.Background(),
		runner,
		repo,
		manifest,
		"adapters/example",
		testReleaseVersion,
		true,
	); err != nil {
		t.Fatalf("strictModule() error = %v", err)
	}

	if !runner.hasGoCommand([]string{"mod", "download"}) {
		t.Error("strictModule() did not run go mod download")
	}
	if !runner.hasGoCommand([]string{"mod", "verify"}) {
		t.Error("strictModule() did not run go mod verify")
	}
	if !runner.hasGoCommand([]string{"test", "-count=1", "./..."}) {
		t.Error("strictModule() did not run uncached tests")
	}
}

func TestStrictAll_ValidTrainChecksEveryModule(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	runner := newSuccessfulReleaseRunner(t)

	if err := strictAll(
		context.Background(),
		runner,
		repo,
		manifest,
		testReleaseVersion,
	); err != nil {
		t.Fatalf("strictAll() error = %v", err)
	}

	downloads := runner.countGoCommand([]string{"mod", "download"})
	if downloads != len(manifest.Published) {
		t.Fatalf("go mod download count = %d, want %d", downloads, len(manifest.Published))
	}
}

func TestStagePublishedModule_RewritesThenStrictlyChecks(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	adapterMod := filepath.Join(repo, "adapters", "example", "go.mod")
	writeTestFile(t, adapterMod, `module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0
replace github.com/mariotoffia/gobridge => ../..
`)
	runner := newSuccessfulReleaseRunner(t)

	if err := stagePublishedModule(
		context.Background(),
		runner,
		repo,
		manifest,
		"adapters/example",
		testReleaseVersion,
		"",
	); err != nil {
		t.Fatalf("stagePublishedModule() error = %v", err)
	}
	data, err := os.ReadFile(adapterMod)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", adapterMod, err)
	}
	text := string(data)
	if !strings.Contains(text, "github.com/mariotoffia/gobridge v0.3.0") {
		t.Errorf("staged go.mod does not contain release version:\n%s", text)
	}
	if strings.Contains(text, "replace") {
		t.Errorf("staged go.mod retained local replacement:\n%s", text)
	}
	if !runner.hasGoCommand([]string{"mod", "tidy"}) {
		t.Error("stagePublishedModule() did not run go mod tidy")
	}
}

func TestStageBootstrapModules_UpdatesRootAndKeepsLocalReplace(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	manifest.Bootstrap = []string{"testutil/wait"}
	helperMod := filepath.Join(repo, "testutil", "wait", "go.mod")
	writeTestFile(t, helperMod, `module github.com/mariotoffia/gobridge/testutil/wait

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0
replace github.com/mariotoffia/gobridge => ../..
`)

	changed, err := stageBootstrapModules(repo, manifest, testReleaseVersion)
	if err != nil {
		t.Fatalf("stageBootstrapModules() error = %v", err)
	}
	if !slices.Equal(changed, []string{"testutil/wait"}) {
		t.Fatalf("changed helpers = %v", changed)
	}
	data, err := os.ReadFile(helperMod)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", helperMod, err)
	}
	text := string(data)
	if !strings.Contains(text, "github.com/mariotoffia/gobridge v0.3.0") {
		t.Errorf("bootstrap go.mod does not require released root:\n%s", text)
	}
	if !strings.Contains(text, "replace github.com/mariotoffia/gobridge => ../..") {
		t.Errorf("bootstrap go.mod lost allowed local replacement:\n%s", text)
	}
}

func TestRunConsumerSmoke_UsesFreshPublicModuleEnvironment(t *testing.T) {
	t.Parallel()

	repo, manifest := writeFixtureRepository(t, false)
	runner := newSuccessfulReleaseRunner(t)
	runner.sideEffect = func(request commandRequest) error {
		if request.Name == "go" && slices.Equal(request.Args, []string{
			"mod",
			"init",
			"example.com/gobridge-release-smoke",
		}) {
			writeTestFile(t, filepath.Join(request.Dir, "go.mod"), `module example.com/gobridge-release-smoke

go 1.25.0

require github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.3.0
`)
		}
		return nil
	}
	// The fixture uses adapters/example in its DAG, while the smoke commands
	// intentionally target the real public Paho path.
	manifest.Published[1].Path = "adapters/mqtt/transport/paho"
	oldDir := filepath.Join(repo, "adapters", "example")
	newDir := filepath.Join(repo, "adapters", "mqtt", "transport", "paho")
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", newDir, err)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("Rename(%s, %s) error = %v", oldDir, newDir, err)
	}
	writeTestFile(t, filepath.Join(newDir, "go.mod"), `module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`)

	if err := runConsumerSmoke(
		context.Background(),
		runner,
		repo,
		manifest,
		"cmd/gobridge/v0.3.0",
	); err != nil {
		t.Fatalf("runConsumerSmoke() error = %v", err)
	}

	getRequest, ok := runner.findGoCommand([]string{
		"get",
		"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@v0.3.0",
	})
	if !ok {
		t.Fatal("consumer smoke did not run exact Paho go get")
	}
	if getRequest.Env["GOWORK"] != "off" || getRequest.Env["GOPROXY"] != publicGoProxy {
		t.Errorf("consumer environment = %#v", getRequest.Env)
	}
	if inside, err := pathIsInside(repo, getRequest.Dir); err != nil {
		t.Fatalf("pathIsInside() error = %v", err)
	} else if inside {
		t.Fatalf("consumer directory %s is inside repository %s", getRequest.Dir, repo)
	}
	if !runner.hasGoCommand([]string{
		"install",
		"github.com/mariotoffia/gobridge/cmd/gobridge@v0.3.0",
	}) {
		t.Fatal("consumer smoke did not install exact cmd/gobridge version")
	}
}

func TestPathIsInside(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	inside := filepath.Join(parent, "child")
	outside := t.TempDir()

	got, err := pathIsInside(parent, inside)
	if err != nil || !got {
		t.Fatalf("pathIsInside(parent, child) = %v, %v; want true, nil", got, err)
	}
	got, err = pathIsInside(parent, outside)
	if err != nil || got {
		t.Fatalf("pathIsInside(parent, outside) = %v, %v; want false, nil", got, err)
	}
}

func fixtureManifest() releaseManifest {
	return releaseManifest{
		Schema:       1,
		ModulePrefix: "github.com/mariotoffia/gobridge",
		Published: []publishedModule{
			{Path: ".", Layer: 0},
			{Path: "adapters/example", Layer: 1},
			{Path: "httpapi", Layer: 2},
			{Path: "cmd/gobridge", Layer: 3},
		},
	}
}

func writeFixtureRepository(t *testing.T, writeManifest bool) (string, releaseManifest) {
	t.Helper()

	repo := t.TempDir()
	manifest := fixtureManifest()
	files := map[string]string{
		"go.mod": `module github.com/mariotoffia/gobridge

go 1.25.0
`,
		"adapters/example/go.mod": `module github.com/mariotoffia/gobridge/adapters/example

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`,
		"httpapi/go.mod": `module github.com/mariotoffia/gobridge/httpapi

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.3.0
	github.com/mariotoffia/gobridge/adapters/example v0.3.0
)
`,
		"cmd/gobridge/go.mod": `module github.com/mariotoffia/gobridge/cmd/gobridge

go 1.25.0

require github.com/mariotoffia/gobridge/httpapi v0.3.0
`,
	}
	for name, content := range files {
		writeTestFile(t, filepath.Join(repo, filepath.FromSlash(name)), content)
	}
	if err := os.MkdirAll(filepath.Join(repo, "processors"), 0o755); err != nil {
		t.Fatalf("MkdirAll(processors) error = %v", err)
	}
	if writeManifest {
		writeManifestFile(t, repo, manifest)
	}
	return repo, manifest
}

func writeManifestFile(t *testing.T, repo string, manifest releaseManifest) {
	t.Helper()

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}
	writeTestFile(t, filepath.Join(repo, filepath.FromSlash(manifestRelativePath)), string(data))
}

func writeTestFile(t *testing.T, filename string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filename, err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", filename, err)
	}
}

func mustReadTestFile(t *testing.T, filename string) []byte {
	t.Helper()

	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", filename, err)
	}
	return data
}

type successfulReleaseRunner struct {
	t          *testing.T
	requests   []commandRequest
	sideEffect func(commandRequest) error
}

func newSuccessfulReleaseRunner(t *testing.T) *successfulReleaseRunner {
	t.Helper()
	return &successfulReleaseRunner{t: t}
}

func (r *successfulReleaseRunner) run(_ context.Context, request commandRequest) ([]byte, error) {
	r.requests = append(r.requests, request)
	if r.sideEffect != nil {
		if err := r.sideEffect(request); err != nil {
			return nil, err
		}
	}

	switch request.Name {
	case "git":
		if slices.Equal(request.Args, []string{"rev-parse", "HEAD"}) ||
			(len(request.Args) == 3 && request.Args[0] == "rev-parse") {
			return []byte("0123456789abcdef0123456789abcdef01234567\n"), nil
		}
		return nil, nil
	case "go":
		if len(request.Args) == 4 &&
			request.Args[0] == "list" &&
			request.Args[1] == "-m" &&
			request.Args[2] == "-json" {
			importPath, version, found := strings.Cut(request.Args[3], "@")
			if !found {
				return nil, fmt.Errorf("invalid module query %q", request.Args[3])
			}
			repo := filepath.Clean(filepath.Join(request.Dir, "..", ".."))
			modulePath, sibling := siblingPath("github.com/mariotoffia/gobridge", importPath)
			goMod := ""
			if sibling {
				goMod = filepath.Join(repo, filepath.FromSlash(modulePath), "go.mod")
				if modulePath == rootModulePath {
					goMod = filepath.Join(repo, "go.mod")
				}
			}
			listed := listedModule{Path: importPath, Version: version, GoMod: goMod}
			listed.Origin.Hash = "0123456789abcdef0123456789abcdef01234567"
			data, err := json.Marshal(listed)
			if err != nil {
				r.t.Fatalf("Marshal(listed module) error = %v", err)
			}
			return data, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %s %v", request.Name, request.Args)
	}
}

func (r *successfulReleaseRunner) hasGoCommand(args []string) bool {
	_, ok := r.findGoCommand(args)
	return ok
}

func (r *successfulReleaseRunner) findGoCommand(args []string) (commandRequest, bool) {
	for _, request := range r.requests {
		if request.Name == "go" && slices.Equal(request.Args, args) {
			return request, true
		}
	}
	return commandRequest{}, false
}

func (r *successfulReleaseRunner) countGoCommand(args []string) int {
	count := 0
	for _, request := range r.requests {
		if request.Name == "go" && slices.Equal(request.Args, args) {
			count++
		}
	}
	return count
}
