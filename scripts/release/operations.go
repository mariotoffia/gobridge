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

	"golang.org/x/mod/module"
)

const publicGoProxy = "https://proxy.golang.org,direct"

type commandRequest struct {
	Dir  string
	Env  map[string]string
	Name string
	Args []string
}

type commandRunner interface {
	run(context.Context, commandRequest) ([]byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, request commandRequest) ([]byte, error) {
	cmd := exec.CommandContext(ctx, request.Name, request.Args...)
	cmd.Dir = request.Dir
	cmd.Env = mergeEnvironment(os.Environ(), request.Env)
	output, err := cmd.CombinedOutput()
	if err != nil {
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

func runModuleChecks(ctx context.Context, runner commandRunner, moduleDir string) error {
	commands := [][]string{
		{"mod", "download"},
		{"mod", "verify"},
		{"build", "./..."},
		{"test", "-count=1", "./..."},
	}
	for _, args := range commands {
		if _, err := runner.run(ctx, commandRequest{
			Dir:  moduleDir,
			Env:  publicModuleEnvironment(),
			Name: "go",
			Args: args,
		}); err != nil {
			return fmt.Errorf("release check %q: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

type repositoryState struct {
	Modules             map[string]moduleManifest
	Violations          []manifestViolation
	BootstrapViolations []manifestViolation
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
	filename := filepath.Join(repo, filepath.FromSlash(modulePath), "go.mod")
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
	result := []string{rootModulePath}
	for _, fixed := range []string{"httpapi", finalModulePath} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(fixed), "go.mod")); err != nil {
			return nil, fmt.Errorf("published module %s: %w", fixed, err)
		}
		result = append(result, fixed)
	}
	for _, tree := range []string{"adapters", "processors"} {
		treeRoot := filepath.Join(repo, tree)
		err := filepath.WalkDir(treeRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Name() != "go.mod" {
				return nil
			}
			dir, err := filepath.Rel(repo, filepath.Dir(filename))
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
		output, err := runner.run(ctx, commandRequest{
			Dir:  filepath.Join(repo, filepath.FromSlash("scripts/release")),
			Env:  publicModuleEnvironment(),
			Name: "go",
			Args: []string{"list", "-m", "-json", query},
		})
		if err != nil {
			return nil, fmt.Errorf(
				"deriving %s pseudo-version after commit %s is reachable: %w",
				importPath,
				commit,
				err,
			)
		}
		var listed listedModule
		if err := json.Unmarshal(output, &listed); err != nil {
			return nil, fmt.Errorf("decoding go list result for %s: %w", importPath, err)
		}
		if listed.Path != importPath {
			return nil, fmt.Errorf("go list returned module %q for %q", listed.Path, importPath)
		}
		if listed.Origin.Hash != commit {
			return nil, fmt.Errorf(
				"go list resolved %s at origin %q, want reachable commit %q",
				importPath,
				listed.Origin.Hash,
				commit,
			)
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
		if listed.GoMod == "" {
			return nil, fmt.Errorf("go list did not report a downloaded go.mod for internal helper %s", importPath)
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
	output, err := runner.run(ctx, commandRequest{
		Dir:  repo,
		Name: "git",
		Args: []string{"rev-parse", "--verify", "refs/tags/" + tag + "^{commit}"},
	})
	if err != nil {
		return fmt.Errorf("resolving release tag %s: %w", tag, err)
	}
	tagCommit := strings.TrimSpace(string(output))
	output, err = runner.run(ctx, commandRequest{
		Dir:  repo,
		Name: "git",
		Args: []string{"rev-parse", "HEAD"},
	})
	if err != nil {
		return fmt.Errorf("resolving release HEAD: %w", err)
	}
	headCommit := strings.TrimSpace(string(output))
	if tagCommit != headCommit {
		return fmt.Errorf("release tag %s points to %s, but checkout HEAD is %s", tag, tagCommit, headCommit)
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
	for _, entry := range manifest.Published {
		if err := verifyDependencyTag(ctx, runner, repo, tagFor(entry.Path, version)); err != nil {
			return fmt.Errorf("release train %s: %w", version, err)
		}
	}
	return nil
}

func verifyDependencyTag(
	ctx context.Context,
	runner commandRunner,
	repo string,
	tag string,
) error {
	output, err := runner.run(ctx, commandRequest{
		Dir:  repo,
		Name: "git",
		Args: []string{"rev-parse", "--verify", "refs/tags/" + tag + "^{commit}"},
	})
	if err != nil {
		return fmt.Errorf("required earlier tag %s is missing: %w", tag, err)
	}
	commit := strings.TrimSpace(string(output))
	if _, err := runner.run(ctx, commandRequest{
		Dir:  repo,
		Name: "git",
		Args: []string{"merge-base", "--is-ancestor", commit, "HEAD"},
	}); err != nil {
		return fmt.Errorf("required earlier tag %s (%s) is not an ancestor of HEAD: %w", tag, commit, err)
	}
	return nil
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
		output, err := runner.run(ctx, commandRequest{
			Dir:  filepath.Join(repo, filepath.FromSlash("scripts/release")),
			Env:  publicModuleEnvironment(),
			Name: "go",
			Args: []string{"list", "-m", "-json", query},
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
		if hasKey(manifest.bootstrapSet(), modulePath) {
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
	output, err := runner.run(ctx, commandRequest{
		Dir:  filepath.Join(repo, filepath.FromSlash("scripts/release")),
		Env:  publicModuleEnvironment(),
		Name: "go",
		Args: []string{"list", "-m", "-json", query},
	})
	if err != nil {
		return fmt.Errorf("published module %s is not publicly resolvable: %w", query, err)
	}
	var listed listedModule
	if err := json.Unmarshal(output, &listed); err != nil {
		return fmt.Errorf("decoding published module resolution for %s: %w", query, err)
	}
	if listed.Path != importPath || listed.Version != version {
		return fmt.Errorf("resolved %s as %s@%s", query, listed.Path, listed.Version)
	}
	if listed.GoMod == "" {
		return fmt.Errorf("published module %s did not report its downloaded go.mod", query)
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
	moduleDir := filepath.Join(repo, filepath.FromSlash(modulePath))
	if modulePath == rootModulePath {
		moduleDir = repo
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
		moduleDir := filepath.Join(repo, filepath.FromSlash(entry.Path))
		if entry.Path == rootModulePath {
			moduleDir = repo
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

	filename := filepath.Join(repo, filepath.FromSlash(modulePath), "go.mod")
	moduleDir := filepath.Dir(filename)
	if modulePath == rootModulePath {
		filename = filepath.Join(repo, "go.mod")
		moduleDir = repo
	}
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
		Dir:  moduleDir,
		Env:  publicModuleEnvironment(),
		Name: "go",
		Args: []string{"mod", "tidy"},
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
		filename := filepath.Join(repo, filepath.FromSlash(modulePath), "go.mod")
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

func runConsumerSmoke(
	ctx context.Context,
	runner commandRunner,
	repo string,
	manifest releaseManifest,
	tag string,
) error {
	version, err := validateSmokeTag(manifest, tag)
	if err != nil {
		return err
	}
	if err := verifyTagAtHead(ctx, runner, repo, tag); err != nil {
		return err
	}
	if err := verifyAllTrainTags(ctx, runner, repo, manifest, version); err != nil {
		return err
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

	environment := publicModuleEnvironment()
	environment["GOBIN"] = filepath.Join(temporary, "bin")
	environment["GOCACHE"] = filepath.Join(temporary, "build-cache")
	environment["GOMODCACHE"] = filepath.Join(temporary, "module-cache")
	for _, directory := range []string{
		environment["GOBIN"],
		environment["GOCACHE"],
		environment["GOMODCACHE"],
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("creating isolated Go directory %s: %w", directory, err)
		}
	}

	paho := manifest.importPath("adapters/mqtt/transport/paho")
	command := manifest.importPath(finalModulePath)
	commands := [][]string{
		{"mod", "init", "example.com/gobridge-release-smoke"},
		{"get", paho + "@" + version},
		{"list", paho},
		{"install", command + "@" + version},
	}
	for _, args := range commands {
		output, err := runner.run(ctx, commandRequest{
			Dir:  temporary,
			Env:  environment,
			Name: "go",
			Args: args,
		})
		if len(output) != 0 {
			if _, err := os.Stdout.Write(output); err != nil {
				return fmt.Errorf("writing external consumer command output: %w", err)
			}
		}
		if err != nil {
			return fmt.Errorf("external consumer command go %s: %w", strings.Join(args, " "), err)
		}
	}

	consumerMod := filepath.Join(temporary, "go.mod")
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
	return nil
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
