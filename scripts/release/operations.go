package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	publicGoProxy      = "https://proxy.golang.org,direct"
	gitCommandTimeout  = 2 * time.Minute
	moduleQueryTimeout = 3 * time.Minute

	// A tag push starts this workflow immediately, so the first resolution of
	// the module the push just created races proxy.golang.org indexing it.
	// The module is correct; the proxy has simply not fetched it yet, and the
	// symptom is an empty GoMod or a plain "not found".
	//
	// Waiting is therefore part of the gate, not a workaround: we wait ON an
	// observable state (the module resolves, reports its go.mod, and its
	// origin matches the tag commit) with time only as the failure budget.
	// Nothing is weakened — every assertion still has to pass, and a module
	// that never appears still fails the release.
	// 20 minutes, not 10, because indexing time is not uniform across modules.
	// A leaf module appears on proxy.golang.org in about a minute, but the two
	// aggregates whose directories contain nested modules — adapters/aws/store
	// and adapters/native/store — consistently took 15 to 20. At 10 minutes
	// they failed their own release workflow on three successive trains while
	// being perfectly correct, and the tag cannot be re-pushed to try again.
	// The budget is a failure deadline, not a target: a module that resolves
	// immediately still costs one poll.
	defaultModulePropagationBudget = 20 * time.Minute
	defaultModulePropagationPoll   = 10 * time.Second
	moduleDownloadLimit            = 10 * time.Minute
	moduleVerifyLimit              = 2 * time.Minute
	moduleBuildLimit               = 15 * time.Minute
	moduleTidyLimit                = 10 * time.Minute
	smokeCommandLimit              = 10 * time.Minute
	smokeOverallLimit              = 25 * time.Minute
)

type commandRequest struct {
	Dir  string
	Env  map[string]string
	Name string
	Args []string
	// Timeout is mandatory for release network/build commands. Zero is reserved
	// for deterministic local helpers and tests.
	Timeout time.Duration
}

type commandRunner interface {
	run(context.Context, commandRequest) ([]byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, request commandRequest) ([]byte, error) {
	runCtx := ctx
	cancel := func() {}
	if request.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, request.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, request.Name, request.Args...)
	cmd.Dir = request.Dir
	cmd.Env = mergeEnvironment(os.Environ(), request.Env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if runCtx.Err() != nil {
			return output, fmt.Errorf(
				"running %s %s in %s: %w",
				request.Name,
				strings.Join(request.Args, " "),
				request.Dir,
				runCtx.Err(),
			)
		}
		return output, fmt.Errorf(
			"running %s %s in %s: %w\n%s",
			request.Name,
			strings.Join(request.Args, " "),
			request.Dir,
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return output, nil
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func publicModuleEnvironment() map[string]string {
	return map[string]string{
		"GOENV":      "off",
		"GOFLAGS":    "",
		"GOINSECURE": "",
		"GONOPROXY":  "none",
		"GONOSUMDB":  "",
		"GOPRIVATE":  "",
		"GOPROXY":    publicGoProxy,
		"GOSUMDB":    "sum.golang.org",
		"GOWORK":     "off",
	}
}

// runModuleChecks proves a published module is consumable: every module in its
// graph is fetchable from the public proxy, the checksums match, and the
// replace-free source compiles against those exact versions.
//
// It deliberately does not run the module's tests. A release tag's commit
// differs from main only in go.mod and go.sum — the Go source is identical —
// so `make test` in CI has already run them on this code. Re-running them here
// tests nothing new about the published artifact, while `mod download`,
// `mod verify` and `build` test the one thing that is new: resolution against
// the rewritten manifests. A consumer never compiles this module's tests, so a
// test failure is a CI concern, not a reason to refuse a tag whose code builds.
//
// `mod download` with no arguments already fetches the whole graph, test
// dependencies included, so `build` adds no downloads — it only proves the
// fetched versions actually compile together.
func runModuleChecks(ctx context.Context, runner commandRunner, moduleDir string) error {
	commands := []struct {
		args    []string
		timeout time.Duration
	}{
		{args: []string{"mod", "download"}, timeout: moduleDownloadLimit},
		{args: []string{"mod", "verify"}, timeout: moduleVerifyLimit},
		{args: []string{"build", "./..."}, timeout: moduleBuildLimit},
	}
	for _, command := range commands {
		if _, err := runner.run(ctx, commandRequest{
			Dir:     moduleDir,
			Env:     publicModuleEnvironment(),
			Name:    "go",
			Args:    command.args,
			Timeout: command.timeout,
		}); err != nil {
			return fmt.Errorf("release check %q: %w", strings.Join(command.args, " "), err)
		}
	}
	return nil
}

type repositoryState struct {
	Modules             map[string]moduleManifest
	Violations          []manifestViolation
	BootstrapViolations []manifestViolation
}

func validateTagPushEvent(created, deleted, forced, protected bool) error {
	if !created {
		return errors.New("release tag event is not a creation")
	}
	if deleted {
		return errors.New("release tag event is a deletion")
	}
	if forced {
		return errors.New("release tag event is forced")
	}
	if !protected {
		return errors.New("release tag is not protected by a tag ruleset")
	}
	return nil
}

func inspectRepository(repo string, manifest releaseManifest, releaseVersion string) (repositoryState, error) {
	if err := validatePublishedSet(repo, manifest); err != nil {
		return repositoryState{}, err
	}

	state := repositoryState{
		Modules: make(map[string]moduleManifest, len(manifest.Published)),
	}
	var errs []error
	usedBootstrap := make(map[string]struct{})
	for _, entry := range manifest.Published {
		moduleFile, err := readModuleManifest(repo, manifest, entry.Path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		state.Modules[entry.Path] = moduleFile
		violations, err := inspectModule(manifest, moduleFile, releaseVersion)
		if err != nil {
			errs = append(errs, err)
		}
		state.Violations = append(state.Violations, violations...)
		recordBootstrapReferences(manifest, moduleFile, usedBootstrap)
	}

	declaredBootstrap := manifest.bootstrapSet()
	for modulePath := range usedBootstrap {
		if !hasKey(declaredBootstrap, modulePath) {
			errs = append(errs, fmt.Errorf("used bootstrap module %q is not declared", modulePath))
		}
	}
	for _, modulePath := range manifest.Bootstrap {
		if !hasKey(usedBootstrap, modulePath) {
			errs = append(errs, fmt.Errorf("declared bootstrap module %q is not referenced by a published manifest", modulePath))
		}
		violations, err := inspectBootstrapModule(repo, manifest, modulePath, releaseVersion)
		if err != nil {
			errs = append(errs, err)
		}
		state.BootstrapViolations = append(state.BootstrapViolations, violations...)
	}

	slices.SortFunc(state.Violations, compareViolations)
	slices.SortFunc(state.BootstrapViolations, compareViolations)
	return state, errors.Join(errs...)
}

func compareViolations(left, right manifestViolation) int {
	leftKey := strings.Join([]string{left.Module, string(left.Kind), left.Dependency, left.Version}, "\x00")
	rightKey := strings.Join([]string{right.Module, string(right.Kind), right.Dependency, right.Version}, "\x00")
	return strings.Compare(leftKey, rightKey)
}

func recordBootstrapReferences(
	manifest releaseManifest,
	moduleFile moduleManifest,
	used map[string]struct{},
) {
	bootstrap := manifest.bootstrapSet()
	for _, requirement := range moduleFile.Requires {
		if modulePath, sibling := siblingPath(manifest.ModulePrefix, requirement.Path); sibling && hasKey(bootstrap, modulePath) {
			used[modulePath] = struct{}{}
		}
	}
	for _, replacement := range moduleFile.Replaces {
		if modulePath, sibling := siblingPath(manifest.ModulePrefix, replacement.OldPath); sibling && hasKey(bootstrap, modulePath) {
			used[modulePath] = struct{}{}
		}
	}
}

func inspectBootstrapModule(
	repo string,
	manifest releaseManifest,
	modulePath string,
	releaseVersion string,
) ([]manifestViolation, error) {
	filename, err := secureJoin(repo, modulePath, "go.mod")
	if err != nil {
		return nil, fmt.Errorf("resolving bootstrap manifest %s: %w", modulePath, err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading bootstrap manifest %s: %w", filename, err)
	}
	parsed, err := parseModuleManifest(filename, data)
	if err != nil {
		return nil, err
	}
	parsed.Path = modulePath
	expectedModule := manifest.importPath(modulePath)
	if parsed.Module != expectedModule {
		return nil, fmt.Errorf("%s declares module %q, want %q", filename, parsed.Module, expectedModule)
	}

	var violations []manifestViolation
	var errs []error
	for _, requirement := range parsed.Requires {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, requirement.Path)
		if !sibling {
			continue
		}
		if dependencyPath != rootModulePath {
			errs = append(errs, fmt.Errorf(
				"bootstrap module %s requires unsupported repository sibling %s",
				modulePath,
				requirement.Path,
			))
			continue
		}
		violations = append(violations, versionViolations(
			modulePath,
			requirement,
			true,
			false,
			releaseVersion,
		)...)
	}
	for _, replacement := range parsed.Replaces {
		if replacement.NewVersion == "" {
			violations = append(violations, manifestViolation{
				Module:     modulePath,
				Kind:       violationLocalReplace,
				Dependency: replacement.OldPath,
				Detail:     replacement.NewPath,
			})
		}
	}
	return violations, errors.Join(errs...)
}

func validatePublishedSet(repo string, manifest releaseManifest) error {
	discovered, err := discoverPublishedModules(repo)
	if err != nil {
		return err
	}
	declared := make([]string, 0, len(manifest.Published))
	for _, entry := range manifest.Published {
		declared = append(declared, entry.Path)
	}
	slices.Sort(discovered)
	slices.Sort(declared)
	if slices.Equal(discovered, declared) {
		return nil
	}

	var missing, extra []string
	for _, modulePath := range discovered {
		if !slices.Contains(declared, modulePath) {
			missing = append(missing, modulePath)
		}
	}
	for _, modulePath := range declared {
		if !slices.Contains(discovered, modulePath) {
			extra = append(extra, modulePath)
		}
	}
	return fmt.Errorf(
		"canonical published set differs from repository policy: missing=%v extra=%v",
		missing,
		extra,
	)
}

func discoverPublishedModules(repo string) ([]string, error) {
	repoRoot, err := secureJoin(repo, rootModulePath)
	if err != nil {
		return nil, fmt.Errorf("resolving repository root: %w", err)
	}
	result := []string{rootModulePath}
	fixedModules := append([]string{"httpapi", finalModulePath}, publishedDeploymentModules...)
	for _, fixed := range fixedModules {
		filename, err := secureJoin(repoRoot, fixed, "go.mod")
		if err != nil {
			return nil, fmt.Errorf("published module %s: %w", fixed, err)
		}
		if _, err := os.Stat(filename); err != nil {
			return nil, fmt.Errorf("published module %s: %w", fixed, err)
		}
		result = append(result, fixed)
	}
	for _, tree := range []string{"adapters", "processors"} {
		treeRoot, err := secureJoin(repoRoot, tree)
		if err != nil {
			return nil, fmt.Errorf("published module tree %s: %w", tree, err)
		}
		err = filepath.WalkDir(treeRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Name() != "go.mod" {
				return nil
			}
			dir, err := filepath.Rel(repoRoot, filepath.Dir(filename))
			if err != nil {
				return fmt.Errorf("finding module path for %s: %w", filename, err)
			}
			result = append(result, filepath.ToSlash(dir))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("discovering published modules under %s: %w", treeRoot, err)
		}
	}
	slices.Sort(result)
	return result, nil
}

type listedModule struct {
	Path    string
	Version string
	GoMod   string
	Origin  struct {
		Hash string
	}
}

func deriveBootstrapVersions(
	ctx context.Context,
	runner commandRunner,
	manifest releaseManifest,
	repo string,
	commit string,
	releaseVersion string,
) (map[string]string, error) {
	if err := validateStableVersion(releaseVersion); err != nil {
		return nil, err
	}
	if !isFullCommitHash(commit) {
		return nil, fmt.Errorf("bootstrap commit %q is not a full 40-character hexadecimal commit", commit)
	}

	versions := make(map[string]string, len(manifest.Bootstrap))
	for _, modulePath := range manifest.Bootstrap {
		importPath := manifest.importPath(modulePath)
		query := importPath + "@" + commit
		toolDir, err := secureJoin(repo, "scripts/release")
		if err != nil {
			return nil, fmt.Errorf("resolving release tool directory: %w", err)
		}
		// Same propagation race as a published tag: the bootstrap commit was
		// pushed moments ago, so the proxy may not have fetched it yet. Wait on
		// the observable state rather than treating "not indexed yet" as a
		// broken helper.
		listed, err := awaitBootstrapResolution(ctx, runner, toolDir, query, importPath, commit)
		if err != nil {
			return nil, err
		}
		if !isUsablePseudoVersion(listed.Version) {
			return nil, fmt.Errorf(
				"go list returned non-pseudo or zero pseudo-version %q for internal helper %s",
				listed.Version,
				importPath,
			)
		}
		if !strings.HasPrefix(commit, mustPseudoRevision(listed.Version)) {
			return nil, fmt.Errorf(
				"go list returned pseudo-version %q whose revision does not match %s",
				listed.Version,
				commit,
			)
		}
		if err := validateResolvedBootstrapGoMod(manifest, listed.GoMod, releaseVersion); err != nil {
			return nil, fmt.Errorf("resolved bootstrap module %s: %w", importPath, err)
		}
		versions[modulePath] = listed.Version
	}
	return versions, nil
}

func isFullCommitHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isUsablePseudoVersion(version string) bool {
	return module.IsPseudoVersion(version) && !isAllZeroPseudoVersion(version)
}

func mustPseudoRevision(version string) string {
	revision, err := module.PseudoVersionRev(version)
	if err != nil {
		panic(fmt.Sprintf("validated pseudo-version %q has no revision: %v", version, err))
	}
	return revision
}

func validateResolvedBootstrapGoMod(
	manifest releaseManifest,
	filename string,
	releaseVersion string,
) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("reading downloaded go.mod %s: %w", filename, err)
	}
	parsed, err := parseModuleManifest(filename, data)
	if err != nil {
		return err
	}
	for _, requirement := range parsed.Requires {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, requirement.Path)
		if !sibling {
			continue
		}
		if dependencyPath != rootModulePath || requirement.Version != releaseVersion {
			return fmt.Errorf(
				"requires %s@%s, want only root module at %s",
				requirement.Path,
				requirement.Version,
				releaseVersion,
			)
		}
	}
	return nil
}

func validateSmokeTag(manifest releaseManifest, tag string) (string, error) {
	entry, version, err := manifest.moduleForTag(tag)
	if err != nil {
		return "", err
	}
	if entry.Path != finalModulePath {
		return "", fmt.Errorf("external consumer smoke requires stable %s tag, got %q", finalModulePath, tag)
	}
	return version, nil
}

func verifyTagAtHead(
	ctx context.Context,
	runner commandRunner,
	repo string,
	tag string,
) error {
	_, err := tagCommitAtHead(ctx, runner, repo, tag)
	return err
}

func tagCommitAtHead(
	ctx context.Context,
	runner commandRunner,
	repo string,
	tag string,
) (string, error) {
	output, err := runner.run(ctx, commandRequest{
		Dir:     repo,
		Name:    "git",
		Args:    []string{"rev-parse", "--verify", "refs/tags/" + tag + "^{commit}"},
		Timeout: gitCommandTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("resolving release tag %s: %w", tag, err)
	}
	tagCommit := strings.TrimSpace(string(output))
	if !isFullCommitHash(tagCommit) {
		return "", fmt.Errorf("release tag %s resolved to invalid commit %q", tag, tagCommit)
	}
	output, err = runner.run(ctx, commandRequest{
		Dir:     repo,
		Name:    "git",
		Args:    []string{"rev-parse", "HEAD"},
		Timeout: gitCommandTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("resolving release HEAD: %w", err)
	}
	headCommit := strings.TrimSpace(string(output))
	if !isFullCommitHash(headCommit) {
		return "", fmt.Errorf("release HEAD resolved to invalid commit %q", headCommit)
	}
	if tagCommit != headCommit {
		return "", fmt.Errorf("release tag %s points to %s, but checkout HEAD is %s", tag, tagCommit, headCommit)
	}
	return tagCommit, nil
}

func parseRemoteTagCommit(tag string, output []byte) (string, error) {
	directRef := "refs/tags/" + tag
	peeledRef := directRef + "^{}"
	direct := ""
	peeled := ""
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !isFullCommitHash(fields[0]) {
			return "", fmt.Errorf("malformed remote tag row %q", line)
		}
		switch fields[1] {
		case directRef:
			if direct != "" {
				return "", fmt.Errorf("remote tag %s has duplicate direct refs", tag)
			}
			direct = fields[0]
		case peeledRef:
			if peeled != "" {
				return "", fmt.Errorf("remote tag %s has duplicate peeled refs", tag)
			}
			peeled = fields[0]
		default:
			return "", fmt.Errorf("remote tag query returned unexpected ref %q", fields[1])
		}
	}
	if direct == "" {
		return "", fmt.Errorf("remote tag %s is missing", tag)
	}
	if peeled != "" {
		return peeled, nil
	}
	return direct, nil
}

func remoteTagCommit(
	ctx context.Context,
	runner commandRunner,
	repo string,
	remote string,
	tag string,
) (string, error) {
	if remote == "" || strings.HasPrefix(remote, "-") {
		return "", fmt.Errorf("git remote %q is invalid", remote)
	}
	for _, char := range remote {
		if (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') &&
			char != '.' && char != '_' && char != '-' {
			return "", fmt.Errorf("git remote %q is invalid", remote)
		}
	}
	output, err := runner.run(ctx, commandRequest{
		Dir:  repo,
		Name: "git",
		Args: []string{
			"ls-remote",
			remote,
			"refs/tags/" + tag,
			"refs/tags/" + tag + "^{}",
		},
		Timeout: gitCommandTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("resolving remote tag %s from %s: %w", tag, remote, err)
	}
	commit, err := parseRemoteTagCommit(tag, output)
	if err != nil {
		return "", fmt.Errorf("resolving remote tag %s from %s: %w", tag, remote, err)
	}
	return commit, nil
}

func verifyRemoteTagCommit(
	ctx context.Context,
	runner commandRunner,
	repo string,
	remote string,
	tag string,
	expectedCommit string,
) error {
	if !isFullCommitHash(expectedCommit) {
		return fmt.Errorf("expected remote tag commit %q is invalid", expectedCommit)
	}
	commit, err := remoteTagCommit(ctx, runner, repo, remote, tag)
	if err != nil {
		return err
	}
	if commit != expectedCommit {
		return fmt.Errorf(
			"remote tag %s on %s points to %s, want validated commit %s",
			tag,
			remote,
			commit,
			expectedCommit,
		)
	}
	return nil
}

func verifyLowerLayerTags(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	target publishedModule,
	version string,
) error {
	for _, dependency := range manifest.Published {
		if dependency.Layer >= target.Layer {
			continue
		}
		if err := verifyDependencyTag(ctx, runner, repo, tagFor(dependency.Path, version)); err != nil {
			return fmt.Errorf("release order for %s: %w", target.Path, err)
		}
	}
	return nil
}

func verifyAllTrainTags(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	version string,
) error {
	_, err := releaseTrainCommits(ctx, runner, repo, manifest, version)
	return err
}

func releaseTrainCommits(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	version string,
) (map[string]string, error) {
	commits := make(map[string]string, len(manifest.Published))
	for _, entry := range manifest.Published {
		commit, err := resolveTagCommit(ctx, runner, repo, tagFor(entry.Path, version))
		if err != nil {
			return nil, fmt.Errorf("release train %s: %w", version, err)
		}
		commits[entry.Path] = commit
	}
	return commits, nil
}

func verifyDependencyTag(
	ctx context.Context,
	runner commandRunner,
	repo string,
	tag string,
) error {
	_, err := resolveTagCommit(ctx, runner, repo, tag)
	return err
}

func resolveTagCommit(
	ctx context.Context,
	runner commandRunner,
	repo string,
	tag string,
) (string, error) {
	output, err := runner.run(ctx, commandRequest{
		Dir:     repo,
		Name:    "git",
		Args:    []string{"rev-parse", "--verify", "refs/tags/" + tag + "^{commit}"},
		Timeout: gitCommandTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("required tag %s is missing: %w", tag, err)
	}
	commit := strings.TrimSpace(string(output))
	if !isFullCommitHash(commit) {
		return "", fmt.Errorf("required tag %s resolved to invalid commit %q", tag, commit)
	}
	if _, err := runner.run(ctx, commandRequest{
		Dir:     repo,
		Name:    "git",
		Args:    []string{"merge-base", "--is-ancestor", commit, "HEAD"},
		Timeout: gitCommandTimeout,
	}); err != nil {
		return "", fmt.Errorf("required tag %s (%s) is not an ancestor of HEAD: %w", tag, commit, err)
	}
	return commit, nil
}

func resolveSiblingRequirements(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	moduleFile moduleManifest,
	releaseVersion string,
) error {
	queries := make(map[string]string)
	for _, requirement := range moduleFile.Requires {
		if _, sibling := siblingPath(manifest.ModulePrefix, requirement.Path); sibling {
			queries[requirement.Path] = requirement.Version
		}
	}
	paths := make([]string, 0, len(queries))
	for importPath := range queries {
		paths = append(paths, importPath)
	}
	slices.Sort(paths)

	for _, importPath := range paths {
		version := queries[importPath]
		query := importPath + "@" + version
		toolDir, err := secureJoin(repo, "scripts/release")
		if err != nil {
			return fmt.Errorf("resolving release tool directory: %w", err)
		}
		output, err := runner.run(ctx, commandRequest{
			Dir:     toolDir,
			Env:     publicModuleEnvironment(),
			Name:    "go",
			Args:    []string{"list", "-m", "-json", query},
			Timeout: moduleQueryTimeout,
		})
		if err != nil {
			return fmt.Errorf("repository sibling %s is not publicly resolvable: %w", query, err)
		}
		var listed listedModule
		if err := json.Unmarshal(output, &listed); err != nil {
			return fmt.Errorf("decoding resolution for %s: %w", query, err)
		}
		if listed.Path != importPath || listed.Version != version {
			return fmt.Errorf(
				"resolved %s as %s@%s",
				query,
				listed.Path,
				listed.Version,
			)
		}
		modulePath, _ := siblingPath(manifest.ModulePrefix, importPath)
		if _, published := manifest.publishedByPath()[modulePath]; published {
			expectedCommit, err := resolveTagCommit(
				ctx,
				runner,
				repo,
				tagFor(modulePath, version),
			)
			if err != nil {
				return err
			}
			if listed.Origin.Hash == "" || listed.Origin.Hash != expectedCommit {
				return fmt.Errorf(
					"published dependency %s resolved from origin %q, want tag commit %s",
					query,
					listed.Origin.Hash,
					expectedCommit,
				)
			}
		} else if hasKey(manifest.bootstrapSet(), modulePath) {
			if !isUsablePseudoVersion(version) {
				return fmt.Errorf("bootstrap dependency %s does not use a valid pseudo-version", query)
			}
			if listed.Origin.Hash == "" || !strings.HasPrefix(listed.Origin.Hash, mustPseudoRevision(version)) {
				return fmt.Errorf(
					"bootstrap dependency %s resolved from unexpected origin %q",
					query,
					listed.Origin.Hash,
				)
			}
			if listed.GoMod == "" {
				return fmt.Errorf("bootstrap dependency %s did not report its downloaded go.mod", query)
			}
			if err := validateResolvedBootstrapGoMod(manifest, listed.GoMod, releaseVersion); err != nil {
				return fmt.Errorf("bootstrap dependency %s: %w", query, err)
			}
		}
	}
	return nil
}

// awaitModuleResolution resolves query from the public proxy, retrying until
// the module reports its downloaded go.mod from the expected tag commit or the
// propagation budget expires.
//
// Every check is the same as a single-shot resolution would apply; the only
// difference is that a not-yet-indexed module is retried instead of failing the
// release. A wrong path, a wrong version, or an origin that does not match the
// tag commit is returned immediately — those are real faults that no amount of
// waiting can fix, and retrying them would only delay a certain failure.
// Overridable so tests exercising the failure path do not sit through the real
// propagation budget. Production never reassigns them.
var (
	modulePropagationBudget = defaultModulePropagationBudget
	modulePropagationPoll   = defaultModulePropagationPoll
)

// awaitBootstrapResolution resolves an internal helper at a just-pushed commit,
// retrying while the proxy has not indexed that commit yet.
//
// Identical reasoning to awaitModuleResolution: a helper whose commit is not
// yet fetched looks exactly like a helper that does not exist, and only one of
// those is a real fault. A resolved module reporting the wrong path or a
// different origin commit is returned immediately — waiting cannot fix either.
func awaitBootstrapResolution(
	ctx context.Context,
	runner commandRunner,
	toolDir string,
	query string,
	importPath string,
	commit string,
) (listedModule, error) {
	deadline := time.Now().Add(modulePropagationBudget)
	var lastErr error
	for attempt := 1; ; attempt++ {
		listed, err, fatal := tryResolveBootstrap(ctx, runner, toolDir, query, importPath, commit)
		if err == nil {
			return listed, nil
		}
		if fatal {
			return listedModule{}, err
		}
		lastErr = err
		if time.Now().After(deadline) {
			return listedModule{}, fmt.Errorf(
				"internal helper %s did not become resolvable at commit %s within %s (%d attempts): %w",
				importPath, commit, modulePropagationBudget, attempt, lastErr,
			)
		}
		fmt.Fprintf(
			os.Stderr,
			"release: waiting for proxy to publish %s (attempt %d): %v\n",
			query, attempt, err,
		)
		select {
		case <-ctx.Done():
			return listedModule{}, ctx.Err()
		case <-time.After(modulePropagationPoll):
		}
	}
}

// tryResolveBootstrap performs one attempt. The bool reports whether the error
// is fatal, meaning retrying cannot help.
func tryResolveBootstrap(
	ctx context.Context,
	runner commandRunner,
	toolDir string,
	query string,
	importPath string,
	commit string,
) (listedModule, error, bool) {
	output, err := runner.run(ctx, commandRequest{
		Dir:     toolDir,
		Env:     publicModuleEnvironment(),
		Name:    "go",
		Args:    []string{"list", "-m", "-json", query},
		Timeout: moduleQueryTimeout,
	})
	if err != nil {
		return listedModule{}, fmt.Errorf(
			"deriving %s pseudo-version after commit %s is reachable: %w", importPath, commit, err,
		), false
	}
	var listed listedModule
	if err := json.Unmarshal(output, &listed); err != nil {
		return listedModule{}, fmt.Errorf("decoding go list result for %s: %w", importPath, err), true
	}
	if listed.Path != importPath {
		return listedModule{}, fmt.Errorf("go list returned module %q for %q", listed.Path, importPath), true
	}
	if listed.Origin.Hash == "" {
		return listedModule{}, fmt.Errorf("go list reported no origin commit for %s", importPath), false
	}
	if listed.Origin.Hash != commit {
		return listedModule{}, fmt.Errorf(
			"go list resolved %s at origin %q, want reachable commit %q", importPath, listed.Origin.Hash, commit,
		), true
	}
	if listed.GoMod == "" {
		return listedModule{}, fmt.Errorf(
			"go list did not report a downloaded go.mod for internal helper %s", importPath,
		), false
	}
	return listed, nil, false
}

func awaitModuleResolution(
	ctx context.Context,
	runner commandRunner,
	toolDir string,
	query string,
	importPath string,
	version string,
	expectedCommit string,
) (listedModule, error) {
	deadline := time.Now().Add(modulePropagationBudget)
	var lastErr error
	for attempt := 1; ; attempt++ {
		listed, err, fatal := tryResolveModule(ctx, runner, toolDir, query, importPath, version, expectedCommit)
		if err == nil {
			return listed, nil
		}
		if fatal {
			return listedModule{}, err
		}
		lastErr = err
		if time.Now().After(deadline) {
			return listedModule{}, fmt.Errorf(
				"published module %s did not become resolvable within %s (%d attempts): %w",
				query, modulePropagationBudget, attempt, lastErr,
			)
		}
		fmt.Fprintf(
			os.Stderr,
			"release: waiting for proxy to publish %s (attempt %d): %v\n",
			query, attempt, err,
		)
		select {
		case <-ctx.Done():
			return listedModule{}, ctx.Err()
		case <-time.After(modulePropagationPoll):
		}
	}
}

// tryResolveModule performs one resolution attempt. The bool reports whether
// the error is fatal, meaning retrying cannot help.
func tryResolveModule(
	ctx context.Context,
	runner commandRunner,
	toolDir string,
	query string,
	importPath string,
	version string,
	expectedCommit string,
) (listedModule, error, bool) {
	output, err := runner.run(ctx, commandRequest{
		Dir:     toolDir,
		Env:     publicModuleEnvironment(),
		Name:    "go",
		Args:    []string{"list", "-m", "-json", query},
		Timeout: moduleQueryTimeout,
	})
	if err != nil {
		// Not indexed yet is indistinguishable from a transient proxy error
		// here, and both are cured by waiting.
		return listedModule{}, fmt.Errorf("published module %s is not publicly resolvable: %w", query, err), false
	}
	var listed listedModule
	if err := json.Unmarshal(output, &listed); err != nil {
		return listedModule{}, fmt.Errorf("decoding published module resolution for %s: %w", query, err), true
	}
	if listed.Path != importPath || listed.Version != version {
		return listedModule{}, fmt.Errorf("resolved %s as %s@%s", query, listed.Path, listed.Version), true
	}
	if listed.GoMod == "" {
		// The proxy answered but has not materialised the module yet.
		return listedModule{}, fmt.Errorf("published module %s did not report its downloaded go.mod", query), false
	}
	if listed.Origin.Hash == "" {
		return listedModule{}, fmt.Errorf("published module %s reported no origin commit", query), false
	}
	if listed.Origin.Hash != expectedCommit {
		// A resolved module from the wrong commit is a real fault: the tag was
		// moved, or a stale cache is serving different content. Fail loudly.
		return listedModule{}, fmt.Errorf(
			"published module %s resolved from origin %q, want tag commit %s",
			query, listed.Origin.Hash, expectedCommit,
		), true
	}
	return listed, nil, false
}

func resolvePublishedModule(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	entry publishedModule,
	version string,
) error {
	importPath := manifest.importPath(entry.Path)
	query := importPath + "@" + version
	expectedCommit, err := resolveTagCommit(ctx, runner, repo, tagFor(entry.Path, version))
	if err != nil {
		return err
	}
	toolDir, err := secureJoin(repo, "scripts/release")
	if err != nil {
		return fmt.Errorf("resolving release tool directory: %w", err)
	}
	listed, err := awaitModuleResolution(ctx, runner, toolDir, query, importPath, version, expectedCommit)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(listed.GoMod)
	if err != nil {
		return fmt.Errorf("reading downloaded go.mod for %s: %w", query, err)
	}
	moduleFile, err := parseModuleManifest(listed.GoMod, data)
	if err != nil {
		return err
	}
	moduleFile.Path = entry.Path
	if moduleFile.Module != importPath {
		return fmt.Errorf("%s declares module %q, want %q", query, moduleFile.Module, importPath)
	}
	violations, err := inspectModule(manifest, moduleFile, version)
	if err != nil {
		return err
	}
	if len(violations) != 0 {
		return violationsError(query, violations)
	}
	return nil
}

func strictModule(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	modulePath string,
	version string,
	checkOwnTag bool,
) error {
	if err := validateStableVersion(version); err != nil {
		return err
	}
	target, ok := manifest.publishedByPath()[modulePath]
	if !ok {
		return fmt.Errorf("module %q is not declared published", modulePath)
	}

	state, err := inspectRepository(repo, manifest, version)
	if err != nil {
		return err
	}
	moduleFile := state.Modules[modulePath]
	violations, err := inspectModule(manifest, moduleFile, version)
	if err != nil {
		return err
	}
	if len(violations) != 0 {
		return violationsError(modulePath, violations)
	}
	if checkOwnTag {
		tag := tagFor(modulePath, version)
		if err := verifyTagAtHead(ctx, runner, repo, tag); err != nil {
			return err
		}
	}
	if err := verifyLowerLayerTags(ctx, runner, repo, manifest, target, version); err != nil {
		return err
	}
	if checkOwnTag {
		if err := resolvePublishedModule(ctx, runner, repo, manifest, target, version); err != nil {
			return err
		}
	}
	if err := resolveSiblingRequirements(ctx, runner, repo, manifest, moduleFile, version); err != nil {
		return err
	}
	moduleDir, err := secureJoin(repo, modulePath)
	if err != nil {
		return fmt.Errorf("resolving module directory %s: %w", modulePath, err)
	}
	return runModuleChecks(ctx, runner, moduleDir)
}

func strictAll(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	version string,
) error {
	if err := validateStableVersion(version); err != nil {
		return err
	}
	state, err := inspectRepository(repo, manifest, version)
	if err != nil {
		return err
	}
	if len(state.Violations) != 0 {
		return violationsError("published module set", state.Violations)
	}
	blockingBootstrap := make([]manifestViolation, 0, len(state.BootstrapViolations))
	for _, violation := range state.BootstrapViolations {
		if violation.Kind != violationLocalReplace {
			blockingBootstrap = append(blockingBootstrap, violation)
		}
	}
	if len(blockingBootstrap) != 0 {
		return violationsError("test-helper bootstrap set", blockingBootstrap)
	}
	if err := verifyAllTrainTags(ctx, runner, repo, manifest, version); err != nil {
		return err
	}
	for _, entry := range manifest.Published {
		if err := resolvePublishedModule(ctx, runner, repo, manifest, entry, version); err != nil {
			return err
		}
	}
	for _, entry := range manifest.Published {
		moduleFile := state.Modules[entry.Path]
		if err := resolveSiblingRequirements(ctx, runner, repo, manifest, moduleFile, version); err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
		moduleDir, err := secureJoin(repo, entry.Path)
		if err != nil {
			return fmt.Errorf("resolving module directory %s: %w", entry.Path, err)
		}
		if err := runModuleChecks(ctx, runner, moduleDir); err != nil {
			return fmt.Errorf("%s: %w", entry.Path, err)
		}
	}
	return nil
}

func violationsError(scope string, violations []manifestViolation) error {
	lines := make([]string, 0, len(violations))
	for _, violation := range violations {
		lines = append(lines, violation.Error())
	}
	return fmt.Errorf("%s is not release-ready:\n  %s", scope, strings.Join(lines, "\n  "))
}

func stagePublishedModule(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	modulePath string,
	version string,
	bootstrapCommit string,
) error {
	target, ok := manifest.publishedByPath()[modulePath]
	if !ok {
		return fmt.Errorf("module %q is not declared published", modulePath)
	}
	if err := validateStableVersion(version); err != nil {
		return err
	}
	if err := verifyLowerLayerTags(ctx, runner, repo, manifest, target, version); err != nil {
		return err
	}

	filename, err := secureJoin(repo, modulePath, "go.mod")
	if err != nil {
		return fmt.Errorf("resolving module manifest %s: %w", modulePath, err)
	}
	moduleDir := filepath.Dir(filename)
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filename, err)
	}
	parsed, err := parseModuleManifest(filename, data)
	if err != nil {
		return err
	}

	bootstrapVersions := existingBootstrapVersions(manifest, parsed)
	if bootstrapCommit != "" {
		bootstrapVersions, err = deriveBootstrapVersions(
			ctx,
			runner,
			manifest,
			repo,
			bootstrapCommit,
			version,
		)
		if err != nil {
			return err
		}
	}
	staged, err := stageModuleManifest(manifest, filename, data, version, bootstrapVersions)
	if err != nil {
		return err
	}
	if err := writeFileAtomically(filename, staged); err != nil {
		return err
	}
	if _, err := runner.run(ctx, commandRequest{
		Dir:     moduleDir,
		Env:     publicModuleEnvironment(),
		Name:    "go",
		Args:    []string{"mod", "tidy"},
		Timeout: moduleTidyLimit,
	}); err != nil {
		return fmt.Errorf("tidying staged module %s: %w", modulePath, err)
	}
	return strictModule(ctx, runner, repo, manifest, modulePath, version, false)
}

func existingBootstrapVersions(
	manifest releaseManifest,
	moduleFile moduleManifest,
) map[string]string {
	bootstrap := manifest.bootstrapSet()
	result := make(map[string]string)
	for _, requirement := range moduleFile.Requires {
		modulePath, sibling := siblingPath(manifest.ModulePrefix, requirement.Path)
		if sibling && hasKey(bootstrap, modulePath) && isUsablePseudoVersion(requirement.Version) {
			result[modulePath] = requirement.Version
		}
	}
	return result
}

func stageBootstrapModules(repo string, manifest releaseManifest, version string) ([]string, error) {
	if err := validateStableVersion(version); err != nil {
		return nil, err
	}
	type stagedFile struct {
		filename string
		data     []byte
	}
	var staged []stagedFile
	var changed []string
	for _, modulePath := range manifest.Bootstrap {
		filename, err := secureJoin(repo, modulePath, "go.mod")
		if err != nil {
			return nil, fmt.Errorf("resolving bootstrap manifest %s: %w", modulePath, err)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", filename, err)
		}
		updated, didChange, err := stageBootstrapManifest(manifest, filename, data, version)
		if err != nil {
			return nil, err
		}
		if didChange {
			staged = append(staged, stagedFile{filename: filename, data: updated})
			changed = append(changed, modulePath)
		}
	}
	for _, file := range staged {
		if err := writeFileAtomically(file.filename, file.data); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func writeFileAtomically(filename string, data []byte) error {
	info, err := os.Stat(filename)
	if err != nil {
		return fmt.Errorf("stating %s: %w", filename, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(filename), "."+filepath.Base(filename)+".release-*")
	if err != nil {
		return fmt.Errorf("creating temporary file for %s: %w", filename, err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryName)
	}()

	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("setting permissions on %s: %w", temporaryName, err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing %s: %w", temporaryName, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("syncing %s: %w", temporaryName, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", temporaryName, err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("replacing %s: %w", filename, err)
	}
	return nil
}

type smokeOptions struct {
	proxyAttempts int
	retryDelay    time.Duration
	wait          func(context.Context, time.Duration) error
}

type smokeCommandError struct {
	err error
}

func (e *smokeCommandError) Error() string {
	return e.err.Error()
}

func (e *smokeCommandError) Unwrap() error {
	return e.err
}

func defaultSmokeOptions() smokeOptions {
	return smokeOptions{
		proxyAttempts: 20,
		retryDelay:    15 * time.Second,
		wait:          waitForRetry,
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, delay)
	defer cancel()
	<-waitCtx.Done()
	if ctx.Err() != nil {
		return fmt.Errorf("waiting for module proxy propagation: %w", ctx.Err())
	}
	return nil
}

func runConsumerSmoke(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	tag string,
) error {
	smokeCtx, cancel := context.WithTimeout(ctx, smokeOverallLimit)
	defer cancel()
	return runConsumerSmokeWithOptions(
		smokeCtx,
		runner,
		repo,
		manifest,
		tag,
		defaultSmokeOptions(),
	)
}

func runConsumerSmokeWithOptions(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	tag string,
	options smokeOptions,
) error {
	version, err := validateSmokeTag(manifest, tag)
	if err != nil {
		return err
	}
	if err := verifyTagAtHead(ctx, runner, repo, tag); err != nil {
		return err
	}
	trainCommits, err := releaseTrainCommits(ctx, runner, repo, manifest, version)
	if err != nil {
		return err
	}
	if options.proxyAttempts < 1 {
		return errors.New("proxy smoke attempts must be positive")
	}
	if options.wait == nil {
		return errors.New("proxy smoke wait function is nil")
	}

	temporary, err := os.MkdirTemp("", "gobridge-external-consumer-*")
	if err != nil {
		return fmt.Errorf("creating external consumer directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(temporary)
	}()
	if inside, err := pathIsInside(repo, temporary); err != nil {
		return err
	} else if inside {
		return fmt.Errorf("consumer smoke directory %s is inside repository %s", temporary, repo)
	}

	var proxyErr error
	for attempt := 1; attempt <= options.proxyAttempts; attempt++ {
		proxyErr = runConsumerSmokePass(
			ctx,
			runner,
			temporary,
			repo,
			manifest,
			version,
			"https://proxy.golang.org",
			fmt.Sprintf("proxy-%02d", attempt),
			trainCommits,
		)
		if proxyErr == nil {
			break
		}
		if attempt == options.proxyAttempts {
			return fmt.Errorf(
				"proxy-only external consumer smoke failed after %d attempts: %w",
				attempt,
				proxyErr,
			)
		}
		var commandErr *smokeCommandError
		if !errors.As(proxyErr, &commandErr) {
			return proxyErr
		}
		if err := options.wait(ctx, options.retryDelay); err != nil {
			return err
		}
	}

	if err := runConsumerSmokePass(
		ctx,
		runner,
		temporary,
		repo,
		manifest,
		version,
		"direct",
		"direct",
		trainCommits,
	); err != nil {
		return fmt.Errorf("direct-only external consumer smoke failed: %w", err)
	}
	return nil
}

func runConsumerSmokePass(
	ctx context.Context,
	runner commandRunner,
	temporaryRoot string,
	repo string,
	manifest releaseManifest,
	version string,
	proxy string,
	passName string,
	trainCommits map[string]string,
) error {
	consumerDir := filepath.Join(temporaryRoot, passName, "consumer")
	environment := isolatedSmokeEnvironment(filepath.Join(temporaryRoot, passName), proxy)
	for _, directory := range []string{
		consumerDir,
		environment["HOME"],
		environment["GOBIN"],
		environment["GOCACHE"],
		environment["GOMODCACHE"],
		environment["GOPATH"],
		environment["XDG_CONFIG_HOME"],
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("creating isolated Go directory %s: %w", directory, err)
		}
	}
	if inside, err := pathIsInside(repo, consumerDir); err != nil {
		return err
	} else if inside {
		return fmt.Errorf("consumer smoke directory %s is inside repository %s", consumerDir, repo)
	}

	paho := manifest.importPath("adapters/mqtt/transport/paho")
	command := manifest.importPath(finalModulePath)
	cdk := manifest.importPath(cdkModulePath)
	if _, err := runner.run(ctx, commandRequest{
		Dir:     consumerDir,
		Env:     environment,
		Name:    "go",
		Args:    []string{"mod", "init", "example.com/gobridge-release-smoke"},
		Timeout: moduleQueryTimeout,
	}); err != nil {
		return fmt.Errorf("external consumer command go mod init: %w", err)
	}
	for _, modulePath := range []string{
		"adapters/mqtt/transport/paho",
		cdkInfraModulePath,
		cdkModulePath,
		finalModulePath,
	} {
		if err := resolveSmokeModule(
			ctx,
			runner,
			consumerDir,
			environment,
			manifest,
			modulePath,
			version,
			trainCommits[modulePath],
		); err != nil {
			return err
		}
	}
	// The CDK modules are not in cmd/gobridge's graph, so nothing else in this
	// smoke compiles them. Build the facade package rather than only listing
	// it: resolution alone would not catch a published module whose stripped
	// manifest no longer satisfies the constructs' own imports.
	//
	// Fetch by PACKAGE path, not module path. `go get module@version` records
	// the requirement but not the go.sum entries for the packages that module's
	// code imports, so a following `go build` fails on every missing sum. The
	// paho pair does not hit this because `go list` needs no build deps, and
	// `go install pkg@version` resolves in module-agnostic mode.
	cdkFacade := cdk + "/" + cdkSmokePackage
	commands := [][]string{
		{"get", paho + "@" + version},
		{"list", paho},
		{"get", cdkFacade + "@" + version},
		{"build", cdkFacade},
		{"install", command + "@" + version},
	}
	for _, args := range commands {
		output, err := runner.run(ctx, commandRequest{
			Dir:     consumerDir,
			Env:     environment,
			Name:    "go",
			Args:    args,
			Timeout: smokeCommandLimit,
		})
		if len(output) != 0 {
			if _, err := os.Stdout.Write(output); err != nil {
				return fmt.Errorf("writing external consumer command output: %w", err)
			}
		}
		if err != nil {
			return &smokeCommandError{err: fmt.Errorf(
				"external consumer command go %s: %w",
				strings.Join(args, " "),
				err,
			)}
		}
	}

	consumerMod := filepath.Join(consumerDir, "go.mod")
	data, err := os.ReadFile(consumerMod)
	if err != nil {
		return fmt.Errorf("reading consumer go.mod: %w", err)
	}
	parsed, err := parseModuleManifest(consumerMod, data)
	if err != nil {
		return err
	}
	if len(parsed.Replaces) != 0 {
		return fmt.Errorf("external consumer go.mod contains %d replace directives", len(parsed.Replaces))
	}
	if len(parsed.Excludes) != 0 {
		return fmt.Errorf("external consumer go.mod contains %d exclude directives", len(parsed.Excludes))
	}
	return nil
}

func isolatedSmokeEnvironment(root, proxy string) map[string]string {
	environment := publicModuleEnvironment()
	environment["GIT_CEILING_DIRECTORIES"] = root
	environment["GIT_CONFIG_COUNT"] = "0"
	environment["GIT_CONFIG_GLOBAL"] = "/dev/null"
	environment["GIT_CONFIG_NOSYSTEM"] = "1"
	environment["GIT_CONFIG_PARAMETERS"] = ""
	environment["GIT_CONFIG_SYSTEM"] = "/dev/null"
	environment["GIT_TERMINAL_PROMPT"] = "0"
	environment["GOBIN"] = filepath.Join(root, "bin")
	environment["GOCACHE"] = filepath.Join(root, "build-cache")
	environment["GOMODCACHE"] = filepath.Join(root, "module-cache")
	environment["GOPATH"] = filepath.Join(root, "gopath")
	environment["GOPROXY"] = proxy
	environment["HOME"] = filepath.Join(root, "home")
	environment["XDG_CONFIG_HOME"] = filepath.Join(root, "xdg")
	return environment
}

func resolveSmokeModule(
	ctx context.Context,
	runner commandRunner,
	dir string,
	environment map[string]string,
	manifest releaseManifest,
	modulePath string,
	version string,
	expectedCommit string,
) error {
	if !isFullCommitHash(expectedCommit) {
		return fmt.Errorf("smoke module %s has invalid expected tag commit %q", modulePath, expectedCommit)
	}
	importPath := manifest.importPath(modulePath)
	query := importPath + "@" + version
	output, err := runner.run(ctx, commandRequest{
		Dir:     dir,
		Env:     environment,
		Name:    "go",
		Args:    []string{"list", "-m", "-json", query},
		Timeout: moduleQueryTimeout,
	})
	if err != nil {
		return &smokeCommandError{err: fmt.Errorf("resolving smoke module %s: %w", query, err)}
	}
	var listed listedModule
	if err := json.Unmarshal(output, &listed); err != nil {
		return fmt.Errorf("decoding smoke resolution for %s: %w", query, err)
	}
	if listed.Path != importPath || listed.Version != version {
		return fmt.Errorf("resolved smoke module %s as %s@%s", query, listed.Path, listed.Version)
	}
	if listed.Origin.Hash == "" || listed.Origin.Hash != expectedCommit {
		return fmt.Errorf(
			"smoke module %s resolved from origin %q, want tag commit %s",
			query,
			listed.Origin.Hash,
			expectedCommit,
		)
	}
	if listed.GoMod == "" {
		return fmt.Errorf("smoke module %s did not report its downloaded go.mod", query)
	}
	data, err := os.ReadFile(listed.GoMod)
	if err != nil {
		return fmt.Errorf("reading smoke module go.mod for %s: %w", query, err)
	}
	moduleFile, err := parseModuleManifest(listed.GoMod, data)
	if err != nil {
		return err
	}
	if len(moduleFile.Replaces) != 0 {
		return fmt.Errorf("smoke module %s contains replace directives", query)
	}
	if len(moduleFile.Excludes) != 0 {
		return fmt.Errorf("smoke module %s contains exclude directives", query)
	}
	return nil
}

func latestStableCommandVersion(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	currentVersion string,
	remote string,
	expectedCommit string,
) (bool, string, error) {
	if err := validateStableVersion(currentVersion); err != nil {
		return false, "", err
	}
	currentTag := tagFor(finalModulePath, currentVersion)
	localCommit, err := tagCommitAtHead(
		ctx,
		runner,
		repo,
		currentTag,
	)
	if err != nil {
		return false, "", fmt.Errorf("re-checking current final-module tag: %w", err)
	}
	if localCommit != expectedCommit {
		return false, "", fmt.Errorf(
			"current final-module tag commit %s does not match validated commit %s",
			localCommit,
			expectedCommit,
		)
	}
	if err := verifyRemoteTagCommit(
		ctx,
		runner,
		repo,
		remote,
		currentTag,
		expectedCommit,
	); err != nil {
		return false, "", fmt.Errorf("re-checking remote final-module tag: %w", err)
	}
	output, err := runner.run(ctx, commandRequest{
		Dir:     repo,
		Name:    "git",
		Args:    []string{"ls-remote", "--tags", remote, "refs/tags/" + finalModulePath + "/v*"},
		Timeout: gitCommandTimeout,
	})
	if err != nil {
		return false, "", fmt.Errorf("listing final-module release tags: %w", err)
	}

	highest := ""
	currentFound := false
	seen := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !isFullCommitHash(fields[0]) {
			return false, "", fmt.Errorf("malformed remote tag listing row %q", line)
		}
		if strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		tag, found := strings.CutPrefix(fields[1], "refs/tags/")
		if !found {
			continue
		}
		entry, version, err := manifest.moduleForTag(tag)
		if err != nil || entry.Path != finalModulePath {
			continue
		}
		if _, duplicate := seen[version]; duplicate {
			continue
		}
		seen[version] = struct{}{}
		if version == currentVersion {
			currentFound = true
		}
		if highest == "" || semver.Compare(version, highest) > 0 {
			highest = version
		}
	}
	if highest == "" {
		return false, "", errors.New("no stable cmd/gobridge release tag exists")
	}
	if !currentFound {
		return false, highest, fmt.Errorf(
			"current version %s has no stable %s tag",
			currentVersion,
			finalModulePath,
		)
	}
	return currentVersion == highest, highest, nil
}

func pathIsInside(parent, child string) (bool, error) {
	parentAbsolute, err := filepath.Abs(parent)
	if err != nil {
		return false, fmt.Errorf("resolving parent path %s: %w", parent, err)
	}
	childAbsolute, err := filepath.Abs(child)
	if err != nil {
		return false, fmt.Errorf("resolving child path %s: %w", child, err)
	}
	relative, err := filepath.Rel(parentAbsolute, childAbsolute)
	if err != nil {
		return false, fmt.Errorf("comparing paths %s and %s: %w", parentAbsolute, childAbsolute, err)
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
