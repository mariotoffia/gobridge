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
	if proxyGetAttempts != 2 || waitCalls != 1 {
		t.Fatalf("proxy attempts=%d waits=%d, want 2 and 1", proxyGetAttempts, waitCalls)
	}

	var getRequests []commandRequest
	for _, request := range requests {
		if request.Name == "go" && len(request.Args) > 0 && request.Args[0] == "get" {
			getRequests = append(getRequests, request)
		}
	}
	if len(getRequests) != 3 {
		t.Fatalf("go get requests = %d, want two proxy attempts and one direct pass", len(getRequests))
	}
	if getRequests[0].Env["GOPROXY"] != "https://proxy.golang.org" ||
		getRequests[1].Env["GOPROXY"] != "https://proxy.golang.org" ||
		getRequests[2].Env["GOPROXY"] != "direct" {
		t.Fatalf("go get proxy order = %q, %q, %q",
			getRequests[0].Env["GOPROXY"],
			getRequests[1].Env["GOPROXY"],
			getRequests[2].Env["GOPROXY"],
		)
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
		return []byte(strings.Join([]string{
			"v0.9.0",
			"cmd/gobridge/v0.3.0",
			"cmd/gobridge/v0.4.0-rc.1",
			"cmd/gobridge/v0.4.0",
			"testutil/wait/v9.9.9",
		}, "\n")), nil
	})

	promote, highest, err := latestStableCommandVersion(
		context.Background(),
		runner,
		"/repo",
		manifest,
		"v0.3.0",
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
		"candidate-${{ github.sha }}",
		"Inspect candidate platform children",
		"Scan linux/amd64 child",
		"Scan linux/arm64 child",
		"release-latest-version",
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
	buildCandidate := strings.Index(text, "Build and push commit-scoped candidate")
	scanArm64 := strings.Index(text, "Scan linux/arm64 child")
	promoteStable := strings.Index(text, "Refuse or resume stable semver tag")
	if buildCandidate < 0 || scanArm64 < buildCandidate || promoteStable < scanArm64 {
		t.Error("stable semver promotion must occur only after candidate build and both child scans")
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
