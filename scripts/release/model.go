package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	manifestRelativePath = "scripts/release/modules.json"
	rootModulePath       = "."
	finalModulePath      = "cmd/gobridge"
)

// publishedDeploymentModules are the only modules under deployment/ that are
// tagged and consumable. Everything else there is internal wiring for the
// shipped image. An external CDK app writes its own stack against the
// constructs, and those constructs take infra types (BootstrapConfig and
// friends) as arguments, so both modules must resolve from the proxy or the
// documented quickstart cannot compile outside this repository.
var publishedDeploymentModules = []string{
	"deployment/aws-filebased-config/cdk",
	"deployment/aws-filebased-config/infra",
}

var (
	stableVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	pseudoLikePattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-.+[0-9]{14}-[^+]+(?:\+incompatible)?$`)
	allZeroRevision      = regexp.MustCompile(`^0+$`)
)

type releaseManifest struct {
	Schema       int               `json:"schema"`
	ModulePrefix string            `json:"module_prefix"`
	Published    []publishedModule `json:"published_modules"`
	Bootstrap    []string          `json:"bootstrap_modules"`
}

type publishedModule struct {
	Path  string `json:"path"`
	Layer int    `json:"layer"`
}

type moduleManifest struct {
	Path     string
	Module   string
	Requires []moduleRequirement
	Replaces []moduleReplacement
	Excludes []moduleRequirement
}

type moduleRequirement struct {
	Path     string
	Version  string
	Indirect bool
}

type moduleReplacement struct {
	OldPath    string
	OldVersion string
	NewPath    string
	NewVersion string
}

type violationKind string

const (
	violationLocalReplace      violationKind = "local-replace"
	violationReplaceDirective  violationKind = "replace-directive"
	violationExcludeDirective  violationKind = "exclude-directive"
	violationExactZero         violationKind = "exact-v0.0.0"
	violationAllZeroPseudo     violationKind = "all-zero-pseudo-version"
	violationMalformedPseudo   violationKind = "malformed-pseudo-version"
	violationVersionMismatch   violationKind = "release-version-mismatch"
	violationBootstrapVersion  violationKind = "bootstrap-version-not-pseudo"
	violationPublishedUnstable violationKind = "published-version-not-stable"
)

type manifestViolation struct {
	Module     string
	Kind       violationKind
	Dependency string
	Version    string
	Detail     string
}

func (v manifestViolation) Error() string {
	target := v.Dependency
	if target == "" {
		target = v.Module
	}
	version := ""
	if v.Version != "" {
		version = "@" + v.Version
	}
	if v.Detail == "" {
		return fmt.Sprintf("%s: %s: %s%s", v.Module, v.Kind, target, version)
	}
	return fmt.Sprintf("%s: %s: %s%s (%s)", v.Module, v.Kind, target, version, v.Detail)
}

func loadManifest(repo string) (releaseManifest, error) {
	filename, err := secureJoin(repo, manifestRelativePath)
	if err != nil {
		return releaseManifest{}, fmt.Errorf("resolving release manifest: %w", err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return releaseManifest{}, fmt.Errorf("reading release manifest %s: %w", filename, err)
	}

	var manifest releaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return releaseManifest{}, fmt.Errorf("decoding release manifest %s: %w", filename, err)
	}
	if err := manifest.validate(); err != nil {
		return releaseManifest{}, fmt.Errorf("validating release manifest %s: %w", filename, err)
	}
	return manifest, nil
}

func (m releaseManifest) validate() error {
	var errs []error
	if m.Schema != 1 {
		errs = append(errs, fmt.Errorf("schema = %d, want 1", m.Schema))
	}
	if err := module.CheckPath(m.ModulePrefix); err != nil {
		errs = append(errs, fmt.Errorf("module_prefix %q: %w", m.ModulePrefix, err))
	}
	if len(m.Published) == 0 {
		errs = append(errs, errors.New("published_modules is empty"))
	}

	published := make(map[string]publishedModule, len(m.Published))
	previousLayer := -1
	previousPath := ""
	maxLayer := -1
	for i, entry := range m.Published {
		if err := validateRelativeModulePath(entry.Path); err != nil {
			errs = append(errs, fmt.Errorf("published_modules[%d]: %w", i, err))
		}
		if entry.Layer < 0 {
			errs = append(errs, fmt.Errorf("published module %q has negative layer %d", entry.Path, entry.Layer))
		}
		if _, exists := published[entry.Path]; exists {
			errs = append(errs, fmt.Errorf("duplicate published module %q", entry.Path))
		}
		published[entry.Path] = entry

		if entry.Layer < previousLayer || (entry.Layer == previousLayer && entry.Path < previousPath) {
			errs = append(errs, errors.New("published_modules must be sorted by layer, then path"))
		}
		if entry.Layer > previousLayer+1 {
			errs = append(errs, fmt.Errorf("published module layers are not contiguous before layer %d", entry.Layer))
		}
		previousLayer = entry.Layer
		previousPath = entry.Path
		maxLayer = max(maxLayer, entry.Layer)
	}

	root, hasRoot := published[rootModulePath]
	if !hasRoot || root.Layer != 0 {
		errs = append(errs, errors.New("root module must be declared at layer 0"))
	}
	final, hasFinal := published[finalModulePath]
	if !hasFinal {
		errs = append(errs, fmt.Errorf("final module %q is not declared", finalModulePath))
	} else if final.Layer != maxLayer {
		errs = append(errs, fmt.Errorf("final module %q is layer %d, want final layer %d", finalModulePath, final.Layer, maxLayer))
	}
	for _, entry := range m.Published {
		if entry.Layer == maxLayer && entry.Path != finalModulePath {
			errs = append(errs, fmt.Errorf("module %q shares final layer %d with %q", entry.Path, maxLayer, finalModulePath))
		}
		if isInternalOnlyPath(entry.Path) {
			errs = append(errs, fmt.Errorf("internal-only module %q is declared published", entry.Path))
		}
	}

	bootstrap := make(map[string]struct{}, len(m.Bootstrap))
	previousPath = ""
	for i, modulePath := range m.Bootstrap {
		if err := validateRelativeModulePath(modulePath); err != nil {
			errs = append(errs, fmt.Errorf("bootstrap_modules[%d]: %w", i, err))
		}
		if !strings.HasPrefix(modulePath, "testutil/") {
			errs = append(errs, fmt.Errorf("bootstrap module %q is not under testutil/", modulePath))
		}
		if _, exists := bootstrap[modulePath]; exists {
			errs = append(errs, fmt.Errorf("duplicate bootstrap module %q", modulePath))
		}
		if _, exists := published[modulePath]; exists {
			errs = append(errs, fmt.Errorf("bootstrap module %q is also declared published", modulePath))
		}
		if modulePath < previousPath {
			errs = append(errs, errors.New("bootstrap_modules must be sorted by path"))
		}
		bootstrap[modulePath] = struct{}{}
		previousPath = modulePath
	}

	return errors.Join(errs...)
}

func validateRelativeModulePath(modulePath string) error {
	if modulePath == rootModulePath {
		return nil
	}
	if modulePath == "" {
		return errors.New("module path is empty")
	}
	if strings.Contains(modulePath, `\`) ||
		strings.IndexFunc(modulePath, func(char rune) bool {
			return unicode.IsControl(char) || unicode.IsSpace(char)
		}) >= 0 ||
		strings.HasPrefix(modulePath, "//") ||
		hasWindowsDrivePrefix(modulePath) ||
		!filepath.IsLocal(filepath.FromSlash(modulePath)) ||
		path.Clean(modulePath) != modulePath ||
		path.IsAbs(modulePath) {
		return fmt.Errorf("module path %q is not a clean repository-relative path", modulePath)
	}
	return nil
}

func hasWindowsDrivePrefix(value string) bool {
	return len(value) >= 2 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':'
}

func isInternalOnlyPath(modulePath string) bool {
	if slices.Contains(publishedDeploymentModules, modulePath) {
		return false
	}
	for _, prefix := range []string{"deployment", "scripts", "tests", "testutil"} {
		if modulePath == prefix || strings.HasPrefix(modulePath, prefix+"/") {
			return true
		}
	}
	return false
}

func validateStableVersion(version string) error {
	if !stableVersionPattern.MatchString(version) {
		return fmt.Errorf("version %q is not stable semantic version vX.Y.Z", version)
	}
	major := semver.Major(version)
	if major != "v0" && major != "v1" {
		return fmt.Errorf(
			"version %q requires a /%s module-path suffix; declared modules support only v0/v1",
			version,
			major,
		)
	}
	return nil
}

func (m releaseManifest) moduleForTag(tag string) (publishedModule, string, error) {
	version := path.Base(tag)
	if err := validateStableVersion(version); err != nil {
		return publishedModule{}, "", fmt.Errorf("tag %q: %w", tag, err)
	}

	modulePath := strings.TrimSuffix(tag, "/"+version)
	if modulePath == tag {
		modulePath = rootModulePath
	}
	entry, ok := m.publishedByPath()[modulePath]
	if !ok {
		if slices.Contains(m.Bootstrap, modulePath) || isInternalOnlyPath(modulePath) {
			return publishedModule{}, "", fmt.Errorf("tag %q targets internal-only module %q", tag, modulePath)
		}
		return publishedModule{}, "", fmt.Errorf("tag %q does not map to a declared published module", tag)
	}
	return entry, version, nil
}

func (m releaseManifest) publishedByPath() map[string]publishedModule {
	result := make(map[string]publishedModule, len(m.Published))
	for _, entry := range m.Published {
		result[entry.Path] = entry
	}
	return result
}

func (m releaseManifest) bootstrapSet() map[string]struct{} {
	result := make(map[string]struct{}, len(m.Bootstrap))
	for _, modulePath := range m.Bootstrap {
		result[modulePath] = struct{}{}
	}
	return result
}

func (m releaseManifest) importPath(modulePath string) string {
	if modulePath == rootModulePath {
		return m.ModulePrefix
	}
	return m.ModulePrefix + "/" + modulePath
}

func tagFor(modulePath, version string) string {
	if modulePath == rootModulePath {
		return version
	}
	return modulePath + "/" + version
}

func parseModuleManifest(filename string, data []byte) (moduleManifest, error) {
	parsed, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return moduleManifest{}, fmt.Errorf("parsing %s: %w", filename, err)
	}
	if parsed.Module == nil {
		return moduleManifest{}, fmt.Errorf("parsing %s: module directive is missing", filename)
	}

	result := moduleManifest{Module: parsed.Module.Mod.Path}
	result.Requires = make([]moduleRequirement, 0, len(parsed.Require))
	for _, requirement := range parsed.Require {
		result.Requires = append(result.Requires, moduleRequirement{
			Path:     requirement.Mod.Path,
			Version:  requirement.Mod.Version,
			Indirect: requirement.Indirect,
		})
	}
	result.Replaces = make([]moduleReplacement, 0, len(parsed.Replace))
	for _, replacement := range parsed.Replace {
		result.Replaces = append(result.Replaces, moduleReplacement{
			OldPath:    replacement.Old.Path,
			OldVersion: replacement.Old.Version,
			NewPath:    replacement.New.Path,
			NewVersion: replacement.New.Version,
		})
	}
	result.Excludes = make([]moduleRequirement, 0, len(parsed.Exclude))
	for _, exclusion := range parsed.Exclude {
		result.Excludes = append(result.Excludes, moduleRequirement{
			Path:    exclusion.Mod.Path,
			Version: exclusion.Mod.Version,
		})
	}
	return result, nil
}

func readModuleManifest(repo string, manifest releaseManifest, modulePath string) (moduleManifest, error) {
	filename, err := secureJoin(repo, modulePath, "go.mod")
	if err != nil {
		return moduleManifest{}, fmt.Errorf("resolving module manifest %s: %w", modulePath, err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return moduleManifest{}, fmt.Errorf("reading %s: %w", filename, err)
	}

	parsed, err := parseModuleManifest(filename, data)
	if err != nil {
		return moduleManifest{}, err
	}
	parsed.Path = modulePath
	expected := manifest.importPath(modulePath)
	if parsed.Module != expected {
		return moduleManifest{}, fmt.Errorf("%s declares module %q, want %q", filename, parsed.Module, expected)
	}
	return parsed, nil
}

func secureJoin(repo, relativePath string, elements ...string) (string, error) {
	if err := validateRelativeModulePath(relativePath); err != nil {
		return "", err
	}
	for _, element := range elements {
		if element == "" || strings.ContainsAny(element, `/\`) || element == "." || element == ".." {
			return "", fmt.Errorf("unsafe path element %q", element)
		}
	}

	rootAbsolute, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolving repository root %s: %w", repo, err)
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbsolute)
	if err != nil {
		return "", fmt.Errorf("resolving repository root symlinks %s: %w", rootAbsolute, err)
	}
	parts := []string{rootResolved}
	if relativePath != rootModulePath {
		parts = append(parts, filepath.FromSlash(relativePath))
	}
	parts = append(parts, elements...)
	candidate := filepath.Join(parts...)
	if err := requirePathWithin(rootResolved, candidate); err != nil {
		return "", err
	}

	probe := candidate
	var missing []string
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspecting path %s: %w", probe, statErr)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing parent for %s", candidate)
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}
	resolvedParent, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", fmt.Errorf("resolving path symlinks %s: %w", probe, err)
	}
	resolved := filepath.Join(append([]string{resolvedParent}, missing...)...)
	if err := requirePathWithin(rootResolved, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func requirePathWithin(root, candidate string) error {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return fmt.Errorf("comparing repository path %s with %s: %w", root, candidate, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s escapes repository root %s", candidate, root)
	}
	return nil
}

func inspectModule(
	manifest releaseManifest,
	moduleFile moduleManifest,
	releaseVersion string,
) ([]manifestViolation, error) {
	current, declared := manifest.publishedByPath()[moduleFile.Path]
	if !declared {
		return nil, fmt.Errorf("module %q is not declared published", moduleFile.Path)
	}
	published := manifest.publishedByPath()
	bootstrap := manifest.bootstrapSet()

	violations := make([]manifestViolation, 0)
	var structuralErrors []error
	for _, requirement := range moduleFile.Requires {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, requirement.Path)
		if !sibling {
			violations = append(violations, versionViolations(
				moduleFile.Path,
				requirement,
				false,
				false,
				"",
			)...)
			continue
		}

		target, isPublished := published[dependencyPath]
		_, isBootstrap := bootstrap[dependencyPath]
		switch {
		case isPublished:
			if target.Layer >= current.Layer {
				structuralErrors = append(structuralErrors, fmt.Errorf(
					"%s requires %s at layer %d; dependencies must be in a lower layer than %d",
					moduleFile.Path,
					dependencyPath,
					target.Layer,
					current.Layer,
				))
			}
		case isBootstrap:
			// Internal test helpers are the only declared pseudo-version exception.
		default:
			structuralErrors = append(structuralErrors, fmt.Errorf(
				"%s requires undeclared repository sibling %s",
				moduleFile.Path,
				dependencyPath,
			))
		}

		violations = append(violations, versionViolations(
			moduleFile.Path,
			requirement,
			isPublished,
			isBootstrap,
			releaseVersion,
		)...)
	}

	for _, replacement := range moduleFile.Replaces {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, replacement.OldPath)
		if sibling {
			if _, isPublished := published[dependencyPath]; !isPublished {
				if _, isBootstrap := bootstrap[dependencyPath]; !isBootstrap {
					structuralErrors = append(structuralErrors, fmt.Errorf(
						"%s replaces undeclared repository sibling %s",
						moduleFile.Path,
						dependencyPath,
					))
				}
			}
		}
		if replacement.NewVersion == "" {
			violations = append(violations, manifestViolation{
				Module:     moduleFile.Path,
				Kind:       violationLocalReplace,
				Dependency: replacement.OldPath,
				Detail:     replacement.NewPath,
			})
		} else {
			violations = append(violations, manifestViolation{
				Module:     moduleFile.Path,
				Kind:       violationReplaceDirective,
				Dependency: replacement.OldPath,
				Version:    replacement.OldVersion,
				Detail:     replacement.NewPath + "@" + replacement.NewVersion,
			})
		}
	}
	for _, exclusion := range moduleFile.Excludes {
		violations = append(violations, manifestViolation{
			Module:     moduleFile.Path,
			Kind:       violationExcludeDirective,
			Dependency: exclusion.Path,
			Version:    exclusion.Version,
		})
	}

	return violations, errors.Join(structuralErrors...)
}

func versionViolations(
	modulePath string,
	requirement moduleRequirement,
	isPublished bool,
	isBootstrap bool,
	releaseVersion string,
) []manifestViolation {
	base := manifestViolation{
		Module:     modulePath,
		Dependency: requirement.Path,
		Version:    requirement.Version,
	}
	var violations []manifestViolation
	switch {
	case requirement.Version == "v0.0.0":
		base.Kind = violationExactZero
		violations = append(violations, base)
	case isAllZeroPseudoVersion(requirement.Version):
		base.Kind = violationAllZeroPseudo
		violations = append(violations, base)
	case looksLikePseudoVersion(requirement.Version) && !module.IsPseudoVersion(requirement.Version):
		base.Kind = violationMalformedPseudo
		violations = append(violations, base)
	}

	if releaseVersion == "" {
		return violations
	}
	switch {
	case isPublished && requirement.Version != releaseVersion:
		base.Kind = violationVersionMismatch
		base.Detail = "want " + releaseVersion
		violations = append(violations, base)
	case isPublished && validateStableVersion(requirement.Version) != nil:
		base.Kind = violationPublishedUnstable
		violations = append(violations, base)
	case isBootstrap && (!module.IsPseudoVersion(requirement.Version) || isAllZeroPseudoVersion(requirement.Version)):
		base.Kind = violationBootstrapVersion
		violations = append(violations, base)
	}
	return violations
}

func siblingPath(prefix, importPath string) (string, bool) {
	if importPath == prefix {
		return rootModulePath, true
	}
	relative, found := strings.CutPrefix(importPath, prefix+"/")
	return relative, found
}

func looksLikePseudoVersion(version string) bool {
	return module.IsPseudoVersion(version) ||
		pseudoLikePattern.MatchString(version) ||
		strings.HasPrefix(version, "v0.0.0-")
}

func isAllZeroPseudoVersion(version string) bool {
	if !module.IsPseudoVersion(version) {
		return false
	}
	revision, err := module.PseudoVersionRev(version)
	if err != nil {
		return false
	}
	timestamp, err := module.PseudoVersionTime(version)
	if err != nil {
		return false
	}
	return allZeroRevision.MatchString(revision) || timestamp.IsZero()
}

func stageModuleManifest(
	manifest releaseManifest,
	filename string,
	data []byte,
	releaseVersion string,
	bootstrapVersions map[string]string,
) ([]byte, error) {
	if err := validateStableVersion(releaseVersion); err != nil {
		return nil, err
	}
	parsed, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}
	if parsed.Module == nil {
		return nil, fmt.Errorf("%s has no module directive", filename)
	}
	published := manifest.publishedByPath()
	bootstrap := manifest.bootstrapSet()

	updateRequirement := func(importPath string) error {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, importPath)
		if !sibling {
			return nil
		}
		switch {
		case dependencyPath == rootModulePath || published[dependencyPath].Path != "":
			return parsed.AddRequire(importPath, releaseVersion)
		case hasKey(bootstrap, dependencyPath):
			version := bootstrapVersions[dependencyPath]
			if !module.IsPseudoVersion(version) || isAllZeroPseudoVersion(version) {
				return fmt.Errorf("bootstrap module %s has invalid derived pseudo-version %q", dependencyPath, version)
			}
			return parsed.AddRequire(importPath, version)
		default:
			return fmt.Errorf("requirement %s is an undeclared repository sibling", importPath)
		}
	}

	for _, requirement := range slices.Clone(parsed.Require) {
		if err := updateRequirement(requirement.Mod.Path); err != nil {
			return nil, fmt.Errorf("updating %s: %w", filename, err)
		}
	}
	for _, replacement := range slices.Clone(parsed.Replace) {
		if replacement.New.Version != "" {
			continue
		}
		if err := updateRequirement(replacement.Old.Path); err != nil {
			return nil, fmt.Errorf("dropping local replacement in %s: %w", filename, err)
		}
		if err := parsed.DropReplace(replacement.Old.Path, replacement.Old.Version); err != nil {
			return nil, fmt.Errorf("dropping local replacement for %s in %s: %w", replacement.Old.Path, filename, err)
		}
	}
	parsed.Cleanup()

	formatted, err := parsed.Format()
	if err != nil {
		return nil, fmt.Errorf("formatting %s: %w", filename, err)
	}
	return formatted, nil
}

func stageBootstrapManifest(
	manifest releaseManifest,
	filename string,
	data []byte,
	releaseVersion string,
) ([]byte, bool, error) {
	if err := validateStableVersion(releaseVersion); err != nil {
		return nil, false, err
	}
	parsed, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return nil, false, fmt.Errorf("parsing %s: %w", filename, err)
	}
	changed := false
	for _, requirement := range parsed.Require {
		dependencyPath, sibling := siblingPath(manifest.ModulePrefix, requirement.Mod.Path)
		if !sibling {
			continue
		}
		if dependencyPath != rootModulePath {
			return nil, false, fmt.Errorf(
				"bootstrap manifest %s requires unsupported repository sibling %s",
				filename,
				requirement.Mod.Path,
			)
		}
		if requirement.Mod.Version != releaseVersion {
			if err := parsed.AddRequire(requirement.Mod.Path, releaseVersion); err != nil {
				return nil, false, fmt.Errorf("updating root requirement in %s: %w", filename, err)
			}
			changed = true
		}
	}
	if !changed {
		return slices.Clone(data), false, nil
	}
	formatted, err := parsed.Format()
	if err != nil {
		return nil, false, fmt.Errorf("formatting %s: %w", filename, err)
	}
	return formatted, true, nil
}

func hasKey[K comparable, V any](values map[K]V, key K) bool {
	_, ok := values[key]
	return ok
}
