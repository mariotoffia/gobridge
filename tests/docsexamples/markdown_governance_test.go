package docsexamples_test

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Three kinds of rot that no compiler, linter, or reader notices until the
// damage is done:
//
//   - a page that outgrew one sitting, so reviewers skim it and edits collide;
//   - a citation to a source LINE, which the next unrelated commit invalidates
//     while still looking authoritative;
//   - a count of things the repository already knows how to count, which is
//     wrong from the first time anyone adds one and says so to nobody.
//
// Category: unit (TESTS.md §1).

// goSourceCitation matches a citation to a Go source line, e.g.
// "runtime/credentials/poll.go:87-108" or "poll.go:184". The FILE is a durable
// reference; the line number stops being true on the next unrelated edit.
var goSourceCitation = regexp.MustCompile(`[A-Za-z0-9_./-]+\.go:\d+(?:[-,]\d+)*`)

// countedDirs are the documentation directories whose contents the repository
// can count for itself. A page that states how many entries one holds has taken
// on a maintenance duty nothing reminds it about.
var countedDirs = map[string]bool{
	"docs/adr":       true,
	"docs/runbooks":  true,
	"docs/scenarios": true,
}

// countPhrase matches a stated cardinality of documentation artifacts —
// "15 records", "18 incident procedures", "Thirty progressive walkthroughs".
var countPhrase = regexp.MustCompile(`(?i)\b(\d{1,3}|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty)\b[^.\n]{0,40}?\b(records?|procedures?|walkthroughs?|scenarios?|runbooks?|pages?|entries|guides?|adrs?)\b`)

// TestGovernedMarkdown_PagesStayReviewable asserts no governed page exceeds the
// review threshold. The fix for a red line here is a split into a hub plus
// topic pages, never a higher threshold.
func TestGovernedMarkdown_PagesStayReviewable(t *testing.T) {
	root := repoRoot(t)

	var oversized []string
	for _, rel := range governedMarkdown(t, root) {
		if reason, exempt := lengthExemptDocs[rel]; exempt {
			require.NotEmpty(t, reason)
			continue
		}
		lines := readGoverned(t, root, rel)
		// A file ending in a newline splits into a trailing empty element that is
		// not a line of prose.
		n := len(lines)
		if n > 0 && lines[n-1] == "" {
			n--
		}
		if n > maxGovernedDocLines {
			oversized = append(oversized, fmt.Sprintf("%s:%d: %d lines (limit %d)", rel, n, n, maxGovernedDocLines))
		}
	}
	sort.Strings(oversized)
	require.Empty(t, oversized,
		"governed pages past the %d-line review threshold; split each into a hub plus topic pages:\n%s",
		maxGovernedDocLines, strings.Join(oversized, "\n"))
}

// prosePages returns every published prose file: the governed Markdown corpus
// plus the AsciiDoc pages under docs/, which rot the same way.
func prosePages(t testing.TB, root string) []string {
	t.Helper()
	pages := governedMarkdown(t, root)
	for _, abs := range walkTreeFiles(t, filepath.Join(root, "docs"), ".adoc") {
		rel, err := filepath.Rel(root, abs)
		require.NoError(t, err)
		pages = append(pages, filepath.ToSlash(rel))
	}
	sort.Strings(pages)
	return pages
}

// TestGovernedMarkdown_CitesNoSourceLineNumbers asserts published prose points
// at a file, symbol, or section — never at a line number.
func TestGovernedMarkdown_CitesNoSourceLineNumbers(t *testing.T) {
	root := repoRoot(t)

	var stale []string
	for _, rel := range prosePages(t, root) {
		lines := readGoverned(t, root, rel)
		fenced := fencedLines(lines)
		for i, line := range lines {
			if fenced[i] {
				continue // a fenced block quotes real compiler or tool output
			}
			for _, cite := range goSourceCitation.FindAllString(line, -1) {
				stale = append(stale, fmt.Sprintf("%s:%d: %s", rel, i+1, cite))
			}
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale,
		"citations to a Go source LINE; the next unrelated commit moves the line and the citation still reads as authoritative. Cite the file, the exported symbol, or the section instead:\n%s",
		strings.Join(stale, "\n"))
}

// TestGovernedMarkdown_StatesNoHandMaintainedCount asserts no page states how
// many entries a countable documentation directory holds.
func TestGovernedMarkdown_StatesNoHandMaintainedCount(t *testing.T) {
	root := repoRoot(t)

	var claims []string
	for _, rel := range governedMarkdown(t, root) {
		lines := readGoverned(t, root, rel)
		fenced := fencedLines(lines)

		start, linksDir := 0, ""
		flush := func(end int) {
			if linksDir != "" {
				block := strings.Join(lines[start:end], " ")
				if m := countPhrase.FindString(block); m != "" {
					claims = append(claims, fmt.Sprintf("%s:%d: %q alongside a link to %s/", rel, start+1, strings.TrimSpace(m), linksDir))
				}
			}
			start, linksDir = end+1, ""
		}
		for i, line := range lines {
			if fenced[i] {
				continue
			}
			if strings.TrimSpace(line) == "" {
				flush(i)
				continue
			}
			for _, m := range markdownLink.FindAllStringSubmatch(stripInlineCode(line), -1) {
				target, _, _ := strings.Cut(m[2], "#")
				resolved := strings.TrimSuffix(path.Join(path.Dir(rel), target), "/")
				if countedDirs[resolved] {
					linksDir = resolved
				}
			}
		}
		flush(len(lines))
	}
	sort.Strings(claims)
	require.Empty(t, claims,
		"prose stating how many entries a documentation directory holds; the number is wrong the first time anyone adds one and nothing says so. Drop the count — the directory listing is the answer:\n%s",
		strings.Join(claims, "\n"))
}

// tableDelimiter matches the row that separates a Markdown table's header from
// its body: "|-----|------|", optionally with alignment colons.
var tableDelimiter = regexp.MustCompile(`^\|(?:\s*:?-{2,}:?\s*\|)+\s*$`)

// TestGovernedMarkdown_TableRowsBelongToATable asserts every run of pipe rows
// starts with a header plus its delimiter.
//
// A row separated from its table by so much as a blank line or a blockquote
// does not render as a row at all: it becomes a paragraph of pipe characters,
// so the setting it documents is invisible on the published page while still
// looking present in the source to whoever greps for it.
func TestGovernedMarkdown_TableRowsBelongToATable(t *testing.T) {
	root := repoRoot(t)

	var detached []string
	for _, rel := range governedMarkdown(t, root) {
		lines := readGoverned(t, root, rel)
		fenced := fencedLines(lines)

		runStart := -1
		closeRun := func(end int) {
			if runStart < 0 {
				return
			}
			run := lines[runStart:end]
			if len(run) < 2 || !tableDelimiter.MatchString(strings.TrimSpace(run[1])) {
				detached = append(detached, fmt.Sprintf("%s:%d: %s", rel, runStart+1, truncate(strings.TrimSpace(run[0]), 90)))
			}
			runStart = -1
		}
		for i, line := range lines {
			isRow := !fenced[i] && strings.HasPrefix(strings.TrimSpace(line), "|")
			switch {
			case isRow && runStart < 0:
				runStart = i
			case !isRow:
				closeRun(i)
			}
		}
		closeRun(len(lines))
	}
	sort.Strings(detached)
	require.Empty(t, detached,
		"table rows with no header row above them; each renders as a paragraph of pipes, so the setting it documents is invisible on the published page:\n%s",
		strings.Join(detached, "\n"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
