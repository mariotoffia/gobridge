package docsexamples_test

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A link is a promise. A relative link whose file was renamed, or an anchor
// whose heading moved to another page during a split, sends a reader to a 404
// or — worse — to the top of a page that no longer contains what they were
// promised, with nothing on screen saying so. Both failures are invisible to
// every other gate in this repository, and both are produced by exactly the
// routine edit this check runs on: renaming a page, splitting a page, or
// rewording a heading.
//
// Category: unit (TESTS.md §1).

// isExternalTarget reports whether a link target leaves the repository, in
// which case resolving it would need the network (TESTS.md §2.5 forbids that).
func isExternalTarget(target string) bool {
	if strings.HasPrefix(target, "//") {
		return true
	}
	for _, scheme := range []string{"http://", "https://", "mailto:", "ftp://", "tel:"} {
		if strings.HasPrefix(strings.ToLower(target), scheme) {
			return true
		}
	}
	return false
}

// TestGovernedMarkdown_RelativeLinksResolve asserts every relative link target
// in the governed corpus names a file or directory that exists.
func TestGovernedMarkdown_RelativeLinksResolve(t *testing.T) {
	root := repoRoot(t)
	files := governedMarkdown(t, root)

	var broken []string
	for _, link := range governedLinks(t, root, files) {
		target, _, _ := strings.Cut(link.target, "#")
		if target == "" || isExternalTarget(link.target) {
			continue
		}
		resolved := path.Join(path.Dir(link.file), target)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved))); err != nil {
			broken = append(broken, fmt.Sprintf("%s:%d: -> %s (resolves to %s)", link.file, link.line, link.target, resolved))
		}
	}
	sort.Strings(broken)
	require.Empty(t, broken,
		"relative links whose target does not exist; each sends a reader to a 404:\n%s",
		strings.Join(broken, "\n"))
}

// TestGovernedMarkdown_AnchorsResolve asserts every "#section" fragment — on
// this page or another — names a heading or explicit anchor that exists there.
func TestGovernedMarkdown_AnchorsResolve(t *testing.T) {
	root := repoRoot(t)
	files := governedMarkdown(t, root)

	anchorsFor := map[string]map[string]bool{}
	lookup := func(rel string) map[string]bool {
		if cached, ok := anchorsFor[rel]; ok {
			return cached
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			anchorsFor[rel] = nil
			return nil
		}
		anchors := pageAnchors(strings.Split(string(body), "\n"))
		anchorsFor[rel] = anchors
		return anchors
	}

	var broken []string
	for _, link := range governedLinks(t, root, files) {
		if isExternalTarget(link.target) {
			continue
		}
		target, fragment, found := strings.Cut(link.target, "#")
		if !found || fragment == "" {
			continue
		}
		page := link.file
		if target != "" {
			page = path.Join(path.Dir(link.file), target)
		}
		if !strings.HasSuffix(page, ".md") {
			continue // a fragment on a non-Markdown target is not ours to slug
		}
		anchors := lookup(page)
		if anchors == nil {
			continue // the missing FILE is already reported by the relative-link check
		}
		// Compared exactly: a browser matches the id as written, so "#Setup-5"
		// does NOT reach a heading GitHub slugged to "setup-5".
		if !anchors[fragment] {
			broken = append(broken, fmt.Sprintf("%s:%d: -> %s (no heading %q on %s)",
				link.file, link.line, link.target, fragment, page))
		}
	}
	sort.Strings(broken)
	require.Empty(t, broken,
		"links whose #fragment names no heading on the target page; each lands a reader at the top of a page that does not answer their question:\n%s",
		strings.Join(broken, "\n"))
}

// TestGovernedMarkdown_EveryPageIsReachable asserts every published page has at
// least one inbound link.
//
// A page nothing links to is invisible: it keeps passing every other check
// while no reader can arrive at it, and it goes stale unnoticed. Splitting a
// long page into topic pages is exactly the edit that produces one — each new
// page lives by the single link the hub gives it.
func TestGovernedMarkdown_EveryPageIsReachable(t *testing.T) {
	root := repoRoot(t)
	files := governedMarkdown(t, root)

	linked := map[string]bool{}
	for _, link := range governedLinks(t, root, files) {
		if isExternalTarget(link.target) {
			continue
		}
		target, _, _ := strings.Cut(link.target, "#")
		if target == "" {
			continue
		}
		linked[path.Join(path.Dir(link.file), target)] = true
	}

	var orphans []string
	for _, rel := range files {
		// An index is an entry point: it is reached by opening the directory.
		switch base := path.Base(rel); {
		case base == "README.md", base == "index.md", strings.HasPrefix(base, "_"):
			continue
		case !strings.Contains(rel, "/"):
			continue // a canonical root document is reached from AGENTS.md's table
		}
		if !linked[rel] {
			orphans = append(orphans, rel)
		}
	}
	sort.Strings(orphans)
	require.Empty(t, orphans,
		"published pages nothing links to; a reader cannot arrive at them and nothing reports them going stale:\n%s",
		strings.Join(orphans, "\n"))
}
