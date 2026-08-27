package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMakeVerifyReleaseTag_DoesNotEvaluateTagAsShell(t *testing.T) {
	t.Parallel()

	repo := repositoryRootForTest(t)
	marker := filepath.Join(t.TempDir(), "executed")
	tag := `v";>` + marker + `;#`
	if err := exec.Command("git", "check-ref-format", "refs/tags/"+tag).Run(); err != nil {
		t.Fatalf("exploit tag must be Git-valid: %v", err)
	}

	cmd := exec.Command(
		"make",
		"--no-print-directory",
		"verify-release-tag",
		"RELEASE_TAG="+tag,
	)
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Error("verify-release-tag accepted a metacharacter tag")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("verify-release-tag executed shell syntax from RELEASE_TAG")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat exploit marker: %v", err)
	}
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()

	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return repo
}

func TestResolvePublishedModule_BindsOriginToTagCommit(t *testing.T) {
	t.Parallel()

	const (
		tagCommit   = "0123456789abcdef0123456789abcdef01234567"
		otherCommit = "fedcba9876543210fedcba9876543210fedcba98"
	)
	repo, manifest := writeFixtureRepository(t, false)
	entry := manifest.publishedByPath()["adapters/example"]
	goMod := filepath.Join(repo, "adapters", "example", "go.mod")

	for _, origin := range []string{"", otherCommit} {
		t.Run("origin_"+origin, func(t *testing.T) {
			t.Parallel()

			runner := qualityRunner(func(_ context.Context, request commandRequest) ([]byte, error) {
				switch request.Name {
				case "git":
					if request.Args[0] == "rev-parse" {
						return []byte(tagCommit + "\n"), nil
					}
					return nil, nil
				case "go":
					listed := listedModule{
						Path:    manifest.importPath(entry.Path),
						Version: testReleaseVersion,
						GoMod:   goMod,
					}
					listed.Origin.Hash = origin
					return json.Marshal(listed)
				default:
					return nil, fmt.Errorf("unexpected command %s", request.Name)
				}
			})

			err := resolvePublishedModule(
				context.Background(),
				runner,
				repo,
				manifest,
				entry,
				testReleaseVersion,
			)
			if err == nil {
				t.Fatalf("resolvePublishedModule() accepted origin %q for tag commit %s", origin, tagCommit)
			}
		})
	}
}

func TestValidateRelativeModulePath_PortableTraversal(t *testing.T) {
	t.Parallel()

	invalid := []string{
		`testutil\..\..\outside`,
		`C:\outside`,
		`C:/outside`,
		`\\server\share`,
		`//server/share`,
		`../outside`,
		`/outside`,
		"adapters/example\nlayer=9",
	}
	for _, modulePath := range invalid {
		if err := validateRelativeModulePath(modulePath); err == nil {
			t.Errorf("validateRelativeModulePath(%q) error = nil", modulePath)
		}
	}
}

func TestReadModuleManifest_RejectsSymlinkParentEscape(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "adapters"), 0o755); err != nil {
		t.Fatalf("mkdir adapters: %v", err)
	}
	writeTestFile(t, filepath.Join(outside, "go.mod"), `module github.com/mariotoffia/gobridge/adapters/escape

go 1.25.0
`)
	if err := os.Symlink(outside, filepath.Join(repo, "adapters", "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifest := fixtureManifest()
	manifest.Published[1].Path = "adapters/escape"

	if _, err := readModuleManifest(repo, manifest, "adapters/escape"); err == nil {
		t.Fatal("readModuleManifest() followed a module parent outside the repository")
	}
}

func TestStableVersion_RejectsIncompatiblePathMajor(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"v2.0.0", "v10.4.3"} {
		if err := validateStableVersion(version); err == nil {
			t.Errorf("validateStableVersion(%q) error = nil", version)
		}
	}
	manifest := fixtureManifest()
	for _, tag := range []string{"v2.0.0", "adapters/example/v2.0.0"} {
		if _, _, err := manifest.moduleForTag(tag); err == nil {
			t.Errorf("moduleForTag(%q) error = nil", tag)
		}
	}
}

func TestExecRunner_EnforcesCommandDeadline(t *testing.T) {
	t.Parallel()

	_, err := (execRunner{}).run(context.Background(), commandRequest{
		Dir:     t.TempDir(),
		Name:    "sh",
		Args:    []string{"-c", "sleep 30"},
		Timeout: 20 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execRunner.run() error = %v, want context deadline exceeded", err)
	}
}

func TestConsumerSmoke_RetriesProxyThenUsesIsolatedDirectPass(t *testing.T) {
	repo, manifest := smokeFixture(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"

	var requests []commandRequest
	proxyGetAttempts := 0
	initCalls := 0
	runner := qualityRunner(func(_ context.Context, request commandRequest) ([]byte, error) {
		requests = append(requests, request)
		switch request.Name {
		case "git":
			if request.Args[0] == "rev-parse" {
				return []byte(commit + "\n"), nil
			}
			return nil, nil
		case "go":
			if slices.Equal(request.Args, []string{"mod", "init", "example.com/gobridge-release-smoke"}) {
				initCalls++
				writeTestFile(t, filepath.Join(request.Dir, "go.mod"), `module example.com/gobridge-release-smoke

go 1.25.0
`)
				return nil, nil
			}
			if len(request.Args) == 4 &&
				slices.Equal(request.Args[:3], []string{"list", "-m", "-json"}) {
				importPath, version, found := strings.Cut(request.Args[3], "@")
				if !found {
					return nil, fmt.Errorf("bad query %q", request.Args[3])
				}
				modulePath, _ := siblingPath(manifest.ModulePrefix, importPath)
				listed := listedModule{
					Path:    importPath,
					Version: version,
					GoMod:   moduleGoModForTest(repo, modulePath),
				}
				listed.Origin.Hash = commit
				return json.Marshal(listed)
			}
			if len(request.Args) > 0 && request.Args[0] == "get" &&
				request.Env["GOPROXY"] == "https://proxy.golang.org" {
				proxyGetAttempts++
				if proxyGetAttempts == 1 {
					return nil, errors.New("proxy has not observed tag")
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected command %s", request.Name)
		}
	})
	waitCalls := 0
	options := smokeOptions{
		proxyAttempts: 2,
		retryDelay:    time.Millisecond,
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}

	if err := runConsumerSmokeWithOptions(
		context.Background(),
		runner,
		repo,
		manifest,
		"cmd/gobridge/v0.3.0",
		options,
	); err != nil {
		t.Fatalf("runConsumerSmokeWithOptions() error = %v", err)
	}
	// Each pass runs `go mod init` once in its own fresh directory, so init
	// calls count passes: two proxy attempts (the first aborted by the
	// injected failure) and one direct pass. Counting passes rather than
	// `go get` calls keeps this independent of how many modules a pass fetches.
	if initCalls != 3 || waitCalls != 1 {
		t.Fatalf("passes=%d waits=%d, want 3 and 1", initCalls, waitCalls)
	}
	if proxyGetAttempts < 2 {
		t.Fatalf("proxy go get attempts = %d, want the failed attempt retried", proxyGetAttempts)
	}

	var getRequests []commandRequest
	for _, request := range requests {
		if request.Name == "go" && len(request.Args) > 0 && request.Args[0] == "get" {
			getRequests = append(getRequests, request)
		}
	}
	if len(getRequests) == 0 {
		t.Fatal("no go get requests issued")
	}
	// The proxy attempts must all precede the direct pass and never resume
	// after it, so the collapsed sequence of GOPROXY values is exactly
	// proxy-then-direct however many modules each pass fetches.
	var proxyOrder []string
	for _, request := range getRequests {
		proxy := request.Env["GOPROXY"]
		if len(proxyOrder) == 0 || proxyOrder[len(proxyOrder)-1] != proxy {
			proxyOrder = append(proxyOrder, proxy)
		}
	}
	if !slices.Equal(proxyOrder, []string{"https://proxy.golang.org", "direct"}) {
		t.Fatalf("go get proxy order = %v, want proxy attempts then one direct pass", proxyOrder)
	}
	homes := make(map[string]struct{})
	caches := make(map[string]struct{})
	for _, request := range getRequests {
		for key, want := range map[string]string{
			"GIT_CONFIG_COUNT":    "0",
			"GIT_CONFIG_GLOBAL":   "/dev/null",
			"GIT_CONFIG_NOSYSTEM": "1",
			"GOSUMDB":             "sum.golang.org",
			"GOWORK":              "off",
		} {
			if request.Env[key] != want {
				t.Errorf("%s = %q, want %q", key, request.Env[key], want)
			}
		}
		if request.Env["HOME"] == "" || request.Env["GOPATH"] == "" {
			t.Errorf("isolated HOME/GOPATH missing: %#v", request.Env)
		}
		if inside, err := pathIsInside(repo, request.Env["GOPATH"]); err != nil || inside {
			t.Errorf("GOPATH %q leaks repository (inside=%v err=%v)", request.Env["GOPATH"], inside, err)
		}
		homes[request.Env["HOME"]] = struct{}{}
		caches[request.Env["GOMODCACHE"]] = struct{}{}
	}
	if len(homes) != 3 || len(caches) != 3 {
		t.Fatalf("HOME/cache isolation counts = %d/%d, want 3/3", len(homes), len(caches))
	}
}

// TestConsumerSmoke_ResolvesAndBuildsPublishedCDK pins the external proof for
// the two CDK modules. Nothing else in the smoke reaches them — cdk is not in
// cmd/gobridge's graph — so the pass has to resolve both against their tag
// commits and actually compile the facade package a third-party stack imports.
// Resolution alone would not catch a published manifest that no longer
// satisfies the constructs' own imports, which is why this asserts `go build`
// and not `go list`.
func TestConsumerSmoke_ResolvesAndBuildsPublishedCDK(t *testing.T) {
	repo, manifest := smokeFixture(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"

	var requests []commandRequest
	runner := qualityRunner(func(_ context.Context, request commandRequest) ([]byte, error) {
		requests = append(requests, request)
		if request.Name != "go" {
			return nil, fmt.Errorf("unexpected command %s", request.Name)
		}
		if slices.Equal(request.Args, []string{"mod", "init", "example.com/gobridge-release-smoke"}) {
			writeTestFile(t, filepath.Join(request.Dir, "go.mod"), `module example.com/gobridge-release-smoke

go 1.25.0
`)
			return nil, nil
		}
		if len(request.Args) == 4 && slices.Equal(request.Args[:3], []string{"list", "-m", "-json"}) {
			importPath, version, found := strings.Cut(request.Args[3], "@")
			if !found {
				return nil, fmt.Errorf("bad query %q", request.Args[3])
			}
			modulePath, _ := siblingPath(manifest.ModulePrefix, importPath)
			listed := listedModule{
				Path:    importPath,
				Version: version,
				GoMod:   moduleGoModForTest(repo, modulePath),
			}
			listed.Origin.Hash = commit
			return json.Marshal(listed)
		}
		return nil, nil
	})

	trainCommits := make(map[string]string, len(manifest.Published))
	for _, entry := range manifest.Published {
		trainCommits[entry.Path] = commit
	}
	err := runConsumerSmokePass(
		context.Background(),
		runner,
		t.TempDir(),
		repo,
		manifest,
		testReleaseVersion,
		"direct",
		"cdk",
		trainCommits,
	)
	if err != nil {
		t.Fatalf("runConsumerSmokePass() error = %v", err)
	}

	cdk := manifest.importPath(cdkModulePath)
	wantResolved := []string{
		manifest.importPath(cdkInfraModulePath) + "@" + testReleaseVersion,
		cdk + "@" + testReleaseVersion,
	}
	var resolved, built, fetched []string
	for _, request := range requests {
		switch {
		case len(request.Args) == 4 && slices.Equal(request.Args[:3], []string{"list", "-m", "-json"}):
			resolved = append(resolved, request.Args[3])
		case len(request.Args) == 2 && request.Args[0] == "build":
			built = append(built, request.Args[1])
		case len(request.Args) == 2 && request.Args[0] == "get":
			fetched = append(fetched, request.Args[1])
		}
	}
	for _, want := range wantResolved {
		if !slices.Contains(resolved, want) {
			t.Errorf("smoke did not resolve %s; resolved %v", want, resolved)
		}
	}
	// Fetching the module path alone leaves go.sum without entries for what the
	// CDK's own code imports, and the build that follows fails on every one of
	// them. The fetch must name the package.
	facade := cdk + "/" + cdkSmokePackage
	if want := facade + "@" + testReleaseVersion; !slices.Contains(fetched, want) {
		t.Errorf("smoke did not go get %s; fetched %v", want, fetched)
	}
	if slices.Contains(fetched, cdk+"@"+testReleaseVersion) {
		t.Errorf("smoke fetched the cdk module path; go build needs the package path")
	}
	if !slices.Contains(built, facade) {
		t.Errorf("smoke did not build %s; built %v", facade, built)
	}
}

func TestLatestStableCommandVersion_DelayedOldTrainCannotPromote(t *testing.T) {
	t.Parallel()

	manifest := fixtureManifest()
	runner := qualityRunner(func(_ context.Context, request commandRequest) ([]byte, error) {
		if request.Name != "git" {
			return nil, fmt.Errorf("unexpected command %s", request.Name)
		}
		if len(request.Args) > 0 && request.Args[0] == "rev-parse" {
			return []byte("0123456789abcdef0123456789abcdef01234567\n"), nil
		}
		if len(request.Args) > 1 && request.Args[1] == "--tags" {
			return []byte(strings.Join([]string{
				"0123456789abcdef0123456789abcdef01234567\trefs/tags/cmd/gobridge/v0.3.0",
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\trefs/tags/cmd/gobridge/v0.4.0-rc.1",
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\trefs/tags/cmd/gobridge/v0.4.0",
			}, "\n")), nil
		}
		return []byte(
			"0123456789abcdef0123456789abcdef01234567\trefs/tags/cmd/gobridge/v0.3.0\n",
		), nil
	})

	promote, highest, err := latestStableCommandVersion(
		context.Background(),
		runner,
		"/repo",
		manifest,
		"v0.3.0",
		"origin",
		"0123456789abcdef0123456789abcdef01234567",
	)
	if err != nil {
		t.Fatalf("latestStableCommandVersion() error = %v", err)
	}
	if promote || highest != "v0.4.0" {
		t.Fatalf("promote=%v highest=%q, want false/v0.4.0", promote, highest)
	}
}

func TestReleaseWorkflow_HardenedPublicationGraph(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	text := string(workflow)
	for _, want := range []string{
		"validate:",
		"external-consumer-smoke:",
		"github-release:",
		"Build and push image content by digest",
		"Inspect image platform children",
		"Scan linux/amd64 child",
		"Scan linux/arm64 child",
		"release-latest-version",
		"Validate or create exact digest association",
		"timeout-minutes:",
		"version: v0.35.0",
		"moby/buildkit:v0.31.1@sha256:",
		"tonistiigi/binfmt:qemu-v10.2.3@sha256:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow missing %q", want)
		}
	}
	releaseJob := strings.Index(text, "\n  github-release:")
	smokeJob := strings.Index(text, "\n  external-consumer-smoke:")
	if releaseJob < 0 || smokeJob < 0 || releaseJob < smokeJob {
		t.Error("GitHub Release job must follow and depend on external smoke")
	}
	if strings.Count(text, "uses: aquasecurity/trivy-action@") != 2 {
		t.Error("release workflow must scan both platform children with pinned Trivy actions")
	}
	buildDigest := strings.Index(text, "Build and push image content by digest")
	scanArm64 := strings.Index(text, "Scan linux/arm64 child")
	promoteLatest := strings.Index(text, "Revalidate associated digest and promote guarded latest")
	if buildDigest < 0 || scanArm64 < buildDigest || promoteLatest < scanArm64 {
		t.Error("latest promotion must occur only after digest-only build and both child scans")
	}
}

func TestWorkflows_AllActionsUseImmutableCommitSHAs(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ci.yml", "release.yml"} {
		workflow, err := os.ReadFile(filepath.Join(
			repositoryRootForTest(t),
			".github",
			"workflows",
			name,
		))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		uses := regexp.MustCompile(`uses:\s+[^@\s]+@([^\s#]+)`).FindAllStringSubmatch(string(workflow), -1)
		if len(uses) == 0 {
			t.Fatalf("%s has no actions", name)
		}
		for _, match := range uses {
			if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(match[1]) {
				t.Errorf("%s action ref %q is not an immutable commit SHA", name, match[1])
			}
		}
	}
}

func TestValidateTagPushEvent_TruthTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		created   bool
		deleted   bool
		forced    bool
		protected bool
		wantErr   bool
	}{
		{name: "protected creation", created: true, protected: true},
		{name: "not created", protected: true, wantErr: true},
		{name: "deleted", created: true, deleted: true, protected: true, wantErr: true},
		{name: "forced", created: true, forced: true, protected: true, wantErr: true},
		{name: "unprotected", created: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateTagPushEvent(tt.created, tt.deleted, tt.forced, tt.protected)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTagPushEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseRemoteTagCommit_LightweightAndAnnotated(t *testing.T) {
	t.Parallel()

	const (
		tag       = "cmd/gobridge/v0.3.0"
		tagObject = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		commit    = "0123456789abcdef0123456789abcdef01234567"
	)
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "lightweight",
			output: commit + "\trefs/tags/" + tag + "\n",
			want:   commit,
		},
		{
			name: "annotated",
			output: tagObject + "\trefs/tags/" + tag + "\n" +
				commit + "\trefs/tags/" + tag + "^{}\n",
			want: commit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseRemoteTagCommit(tag, []byte(tt.output))
			if err != nil {
				t.Fatalf("parseRemoteTagCommit() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseRemoteTagCommit() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyRemoteTagCommit_RejectsMovedOrMissingTag(t *testing.T) {
	t.Parallel()

	const (
		tag      = "cmd/gobridge/v0.3.0"
		expected = "0123456789abcdef0123456789abcdef01234567"
		moved    = "fedcba9876543210fedcba9876543210fedcba98"
	)
	for _, output := range []string{"", moved + "\trefs/tags/" + tag + "\n"} {
		runner := qualityRunner(func(_ context.Context, request commandRequest) ([]byte, error) {
			if request.Name != "git" {
				return nil, fmt.Errorf("unexpected command %s", request.Name)
			}
			return []byte(output), nil
		})
		if err := verifyRemoteTagCommit(
			context.Background(),
			runner,
			"/repo",
			"origin",
			tag,
			expected,
		); err == nil {
			t.Fatalf("verifyRemoteTagCommit() accepted remote output %q", output)
		}
	}
}

func TestReleaseWorkflow_DigestOnlyAndProtectedTagPolicy(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	text := string(workflow)
	for _, want := range []string{
		"github.event.created",
		"github.event.deleted",
		"github.event.forced",
		"github.ref_protected",
		"push-by-digest=true",
		"name-canonical=true",
		"gobridge-image-digest.txt",
		"verify-remote-release-tag",
		"tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0",
		"moby/buildkit:v0.31.1@sha256:6b59b7df63a8cb9902736f9ddf7fcff8261613d3e7449b8ea8b7537fc399c03a",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"candidate-${{ github.sha }}",
		"Refuse or resume stable semver tag",
		"tags: ghcr.io/mariotoffia/gobridge:",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("release workflow retains mutable image tag behavior %q", forbidden)
		}
	}
}

func smokeFixture(t *testing.T) (string, releaseManifest) {
	t.Helper()

	repo, manifest := writeFixtureRepository(t, false)
	manifest.Published[1].Path = "adapters/mqtt/transport/paho"
	oldDir := filepath.Join(repo, "adapters", "example")
	newDir := filepath.Join(repo, "adapters", "mqtt", "transport", "paho")
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		t.Fatalf("mkdir paho parent: %v", err)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("rename fixture adapter: %v", err)
	}
	writeTestFile(t, filepath.Join(newDir, "go.mod"), `module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require github.com/mariotoffia/gobridge v0.3.0
`)
	return repo, manifest
}

func moduleGoModForTest(repo, modulePath string) string {
	if modulePath == rootModulePath {
		return filepath.Join(repo, "go.mod")
	}
	return filepath.Join(repo, filepath.FromSlash(modulePath), "go.mod")
}

type qualityRunner func(context.Context, commandRequest) ([]byte, error)

func (run qualityRunner) run(ctx context.Context, request commandRequest) ([]byte, error) {
	return run(ctx, request)
}
