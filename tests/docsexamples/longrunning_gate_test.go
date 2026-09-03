package docsexamples_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The long-running suite carries a build tag no default `go test`, `go vet` or
// `golangci-lint` invocation supplies, so the repository's module walks list no
// packages for it and skip it entirely. Three things can then rot unseen:
//
//   - the suite stops COMPILING, and every production proof in it is gone while
//     the branch stays green;
//   - the release gate SELECTS test names that no longer exist — `-run` matching
//     nothing exits 0, so a renamed proof leaves the gate reporting success
//     having run nothing;
//   - a test's exercised message count drifts from the volume its table row
//     advertises, so release evidence overstates the workload.
//
// These checks close all three from the default build, which is the only place
// a pull request looks.
//
// Category: unit (TESTS.md §1).

const longRunningDir = "tests/longrunning"

// TestLongRunningSuite_IsCompiledByEveryLintRun requires `make lint` to vet the
// long-running module under its own build tag. Vetting type-checks every file,
// so a refactor that breaks the suite fails the branch instead of surfacing
// hours later on a developer machine.
func TestLongRunningSuite_IsCompiledByEveryLintRun(t *testing.T) {
	recipe := makefileRecipe(t, "lint")

	require.Truef(t, strings.Contains(recipe, longRunningDir),
		"the lint target must reach %s; nothing else compiles it", longRunningDir)
	require.True(t, strings.Contains(recipe, "-tags=longrunning"),
		"the long-running module has no default-tag packages, so an untagged vet lists nothing")
}

// TestReleaseGate_SelectsProofsThatExist pins every test name in the release
// subset against the suite. The subset is the evidence a release rests on, and
// `go test -run` treats an unmatched pattern as success.
func TestReleaseGate_SelectsProofsThatExist(t *testing.T) {
	root := repoRoot(t)
	names := makefileList(t, "RELEASE_LONGRUNNING_TESTS")
	require.NotEmpty(t, names, "the release subset must name the proofs it runs")

	declared := longRunningTestNames(t, root)
	for _, name := range names {
		require.Truef(t, slices.Contains(declared, name),
			"release subset names %s, which no test in %s declares", name, longRunningDir)
	}
}

// TestLongRunningVolumes_MatchTheirDescription requires each use-case test's
// message-count constant to be stated somewhere in its README row. A
// description that overstates the exercised workload turns the suite's own
// report into misleading release evidence.
//
// The whole row counts, not just the Volume cell, because a row legitimately
// splits its volume across cells — "2,000 | Alpha=1,000, Beta=1,000" describes
// a per-session count of 1,000 truthfully. The check therefore catches a
// contradiction, not every arithmetic relationship: it asks whether the number
// the test really sends appears in what a reviewer reads.
func TestLongRunningVolumes_MatchTheirDescription(t *testing.T) {
	root := repoRoot(t)
	advertised := readmeRowNumbers(t, root)
	exercised := longRunningVolumes(t, root)
	require.NotEmpty(t, exercised, "no use-case message counts were found to check")

	var (
		checked   int
		disagreed []string
	)
	for _, useCase := range sortedKeys(exercised) {
		stated, documented := advertised[useCase]
		if !documented {
			continue
		}
		checked++
		if !slices.Contains(stated, exercised[useCase]) {
			disagreed = append(disagreed, fmt.Sprintf(
				"UC%s exercises %d, README row states %v", useCase, exercised[useCase], stated))
		}
	}
	require.Positive(t, checked, "no use case had both a count constant and a README row")
	require.Empty(t, disagreed, "message counts must match the volume their table row advertises")
}

// TestFuzzTargets_SelectedByNameExist pins every target the fuzz gate names
// against its package. `go test -fuzz` on a pattern that matches nothing exits
// 0, so a renamed target silently removes mutation coverage from the gate.
func TestFuzzTargets_SelectedByNameExist(t *testing.T) {
	root := repoRoot(t)
	entries := makefileList(t, "FUZZ_TARGETS")
	require.NotEmpty(t, entries, "the fuzz gate must name the targets it mutates")

	for _, entry := range entries {
		dir, target, ok := strings.Cut(entry, ":")
		require.Truef(t, ok, "fuzz entry %q must be <package directory>:<target>", entry)
		require.Truef(t, strings.HasPrefix(target, "Fuzz"), "%q is not a fuzz target", target)

		files, err := filepath.Glob(filepath.Join(root, dir, "*_test.go"))
		require.NoError(t, err)
		require.NotEmptyf(t, files, "fuzz entry %q names a directory with no tests", entry)

		found := false
		fset := token.NewFileSet()
		for _, file := range files {
			parsed, parseErr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
			require.NoError(t, parseErr, file)
			for _, decl := range parsed.Decls {
				fn, isFunc := decl.(*ast.FuncDecl)
				if isFunc && fn.Recv == nil && fn.Name.Name == target {
					found = true
				}
			}
		}
		require.Truef(t, found, "fuzz gate names %s, which %s does not declare", target, dir)
	}
}

// ---------------------------------------------------------------------------
// Makefile reading
// ---------------------------------------------------------------------------

// makefileRecipe returns the recipe lines of one Makefile target, joined by
// newlines. Scoping to the target is the point: a check that searched the whole
// file would pass on a command parked in an unrelated target.
func makefileRecipe(t testing.TB, target string) string {
	t.Helper()
	var (
		recipe    []string
		collected bool
		inTarget  bool
	)
	for _, line := range makefileLines(t) {
		switch {
		case strings.HasPrefix(line, target+":"):
			inTarget = true
			collected = true
		case inTarget && strings.HasPrefix(line, "\t"):
			// A recipe line starts with a tab, and only a recipe line does.
			// Accepting anything else would keep collecting past the target and
			// let a command in a NEIGHBOURING one satisfy this check.
			recipe = append(recipe, line)
		case inTarget:
			inTarget = false
		}
	}
	require.True(t, collected, "Makefile declares no %s target", target)
	return strings.Join(recipe, "\n")
}

// makefileList returns the whitespace-separated values of a Makefile variable,
// following backslash continuations.
func makefileList(t testing.TB, variable string) []string {
	t.Helper()
	lines := makefileLines(t)
	for i, line := range lines {
		_, value, found := strings.Cut(line, ":=")
		if !found || strings.TrimSpace(strings.Split(line, ":=")[0]) != variable {
			continue
		}
		joined := value
		for strings.HasSuffix(strings.TrimSpace(joined), `\`) && i+1 < len(lines) {
			joined = strings.TrimSuffix(strings.TrimSpace(joined), `\`) + " " + lines[i+1]
			i++
		}
		return strings.Fields(joined)
	}
	t.Fatalf("Makefile declares no %s variable", variable)
	return nil
}

func makefileLines(t testing.TB) []string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	require.NoError(t, err)
	return strings.Split(string(source), "\n")
}

// ---------------------------------------------------------------------------
// Long-running suite reading
// ---------------------------------------------------------------------------

func longRunningTestNames(t testing.TB, root string) []string {
	t.Helper()
	var names []string
	forEachLongRunningFile(t, root, func(file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names = append(names, fn.Name.Name)
			}
		}
	})
	sort.Strings(names)
	return names
}

// useCaseCount matches a message-count constant belonging to a numbered use
// case, either at file scope (`uc1MsgCount`) or inside the test function
// (`msgCount`, taken from a `TestUC42_...` body).
var (
	scopedCount   = regexp.MustCompile(`^uc(\d+)MsgCount$`)
	functionUC    = regexp.MustCompile(`^TestUC(\d+)[_A-Za-z0-9]*$`)
	localCountVar = regexp.MustCompile(`^(?:msgCount|msgsPerRule)$`)

	// A file-scope constant wins over a function-local one: `uc1MsgCount` is
	// the count UC1 declares for itself, and nothing else in its file may
	// silently override it.
)

// longRunningVolumes maps a use-case number to the message count its test
// actually exercises.
func longRunningVolumes(t testing.TB, root string) map[string]int {
	t.Helper()
	volumes := map[string]int{}
	scoped := map[string]bool{}
	forEachLongRunningFile(t, root, func(file *ast.File) {
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.GenDecl:
				collectConstants(node, func(name string, value int) {
					if m := scopedCount.FindStringSubmatch(name); m != nil {
						volumes[m[1]] = value
						scoped[m[1]] = true
					}
				})
			case *ast.FuncDecl:
				m := functionUC.FindStringSubmatch(node.Name.Name)
				if m == nil || node.Body == nil {
					continue
				}
				ast.Inspect(node.Body, func(n ast.Node) bool {
					gen, ok := n.(*ast.GenDecl)
					if !ok {
						return true
					}
					collectConstants(gen, func(name string, value int) {
						if localCountVar.MatchString(name) && !scoped[m[1]] {
							volumes[m[1]] = value
						}
					})
					return true
				})
			}
		}
	})
	return volumes
}

// sortedKeys returns map keys in a stable order so a failure lists the same
// use cases in the same sequence on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func collectConstants(decl *ast.GenDecl, visit func(name string, value int)) {
	if decl.Tok != token.CONST {
		return
	}
	for _, spec := range decl.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range value.Names {
			if i >= len(value.Values) {
				continue
			}
			literal, ok := value.Values[i].(*ast.BasicLit)
			if !ok || literal.Kind != token.INT {
				continue
			}
			parsed, err := strconv.Atoi(strings.ReplaceAll(literal.Value, "_", ""))
			if err == nil {
				visit(name.Name, parsed)
			}
		}
	}
}

func forEachLongRunningFile(t testing.TB, root string, visit func(*ast.File)) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(root, longRunningDir, "*_test.go"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "no long-running test files found")

	fset := token.NewFileSet()
	for _, entry := range entries {
		parsed, parseErr := parser.ParseFile(fset, entry, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, entry)
		visit(parsed)
	}
}

// ---------------------------------------------------------------------------
// README reading
// ---------------------------------------------------------------------------

var (
	readmeRow = regexp.MustCompile(`^\|\s*UC(\d+)\s*\|(.*)$`)
	volumeNum = regexp.MustCompile(`[\d,]+`)
)

// readmeRowNumbers maps a use-case number to every number stated anywhere in
// its README table row.
func readmeRowNumbers(t testing.TB, root string) map[string][]int {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(root, longRunningDir, "README.md"))
	require.NoError(t, err)

	volumes := map[string][]int{}
	for _, line := range strings.Split(string(source), "\n") {
		m := readmeRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var stated []int
		for _, raw := range volumeNum.FindAllString(m[2], -1) {
			parsed, convErr := strconv.Atoi(strings.ReplaceAll(raw, ",", ""))
			if convErr == nil {
				stated = append(stated, parsed)
			}
		}
		if len(stated) > 0 {
			volumes[m[1]] = stated
		}
	}
	return volumes
}

// TestBrokerSupportMatrix_NamesEvidenceThatExists pins the published
// broker-feature matrix against the suite. The page's whole value is that every
// claim points at a test which fails if the behaviour regresses; a claim whose
// evidence was renamed or deleted is worse than an unsupported claim, because
// it reads as proved.
func TestBrokerSupportMatrix_NamesEvidenceThatExists(t *testing.T) {
	root := repoRoot(t)
	page := filepath.Join(root, "docs", "transports", "mqtt-broker-support.md")
	source, err := os.ReadFile(page)
	require.NoError(t, err)

	declared := map[string]bool{}
	for _, name := range longRunningTestNames(t, root) {
		declared[name] = true
	}
	for _, dir := range []string{
		filepath.Join("adapters", "mqtt", "transport", "paho"),
		filepath.Join("tests", "integration"),
	} {
		for _, name := range testNamesIn(t, filepath.Join(root, dir)) {
			declared[name] = true
		}
	}

	cited := citedTestName.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, cited, "the broker matrix must cite the tests it rests on")

	var missing []string
	for _, match := range cited {
		if !declared[match[1]] {
			missing = append(missing, match[1])
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"the broker support matrix cites tests nothing declares:\n%s", strings.Join(missing, "\n"))
}

// citedTestName matches a backticked test-function name in prose.
var citedTestName = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")

func testNamesIn(t testing.TB, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	require.NoError(t, err)

	var names []string
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, parseErr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, file)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}
