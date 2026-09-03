package docsexamples_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Shared corpus for the published-Markdown governance checks.
//
// "Governed" means prose this repository publishes and maintains: everything
// under docs/ plus the canonical root documents AGENTS.md points readers at. A
// planning worklist is deliberately NOT governed — it is written to be deleted,
// its links point at source lines that move, and holding it to the same
// structure rules would only slow the work down.
//
// Category: unit (TESTS.md §1) — the Markdown files are the fixtures; no
// process, container, or clock is involved.

// governedRootDocs are the canonical root documents. AGENTS.md routes every
// task to one of these, so a broken link or a hidden section in one of them
// costs a reader the same as a broken link under docs/.
var governedRootDocs = []string{
	"AGENTS.md",
	"ARCHITECTURE.md",
	"DDD.md",
	"DEVELOPMENT.md",
	"LANGUAGE.md",
	"LINT.md",
	"MODULES.md",
	"PLUGIN.md",
	"README.md",
	"RELEASE.md",
	"TESTS.md",
	"UBIQUITOUS.md",
}

// lengthExemptDocs are governed for links and anchors but not for length,
// because their length is a property of what they are rather than a symptom of
// a page that needs splitting.
var lengthExemptDocs = map[string]string{
	// A release log only grows; splitting it by size would cut a release in half.
	"CHANGELOG.md": "append-only release log",
	// The glossary is append-only by rule: a correction is a new row, never an
	// edit to an old one, so it can only grow.
	"UBIQUITOUS.md": "append-only glossary",
}

// maxGovernedDocLines is the review threshold. A page longer than this cannot
// be reviewed in one sitting, and edits to it collide; the fix is to split it
// into a hub plus topic pages, not to raise the number.
const maxGovernedDocLines = 500

// governedMarkdown returns the repo-relative, sorted set of governed Markdown
// files: every page under docs/ plus the canonical root documents.
func governedMarkdown(t testing.TB, root string) []string {
	t.Helper()

	files := markdownFiles(t, root) // docs/** + README.md
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}
	for _, name := range governedRootDocs {
		if seen[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			continue // a canonical doc that does not exist is LINT.md's problem, not this check's
		}
		files = append(files, name)
		seen[name] = true
	}
	// CHANGELOG.md is linked from README and the release notes; it is governed
	// for links even though it is exempt from the length rule.
	if _, err := os.Stat(filepath.Join(root, "CHANGELOG.md")); err == nil && !seen["CHANGELOG.md"] {
		files = append(files, "CHANGELOG.md")
	}
	sort.Strings(files)
	require.NotEmpty(t, files, "no governed Markdown files found under %s", root)
	return files
}

// readGoverned reads one governed page and returns its lines.
func readGoverned(t testing.TB, root, rel string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err, "reading governed page %s", rel)
	return strings.Split(string(body), "\n")
}

// fencedLines reports, per line index, whether that line sits inside a fenced
// code block (the fence markers themselves count as inside). A link, a heading,
// or a stale source citation inside a fence is a sample of something else's
// syntax, not a claim this repository makes.
func fencedLines(lines []string) []bool {
	inside := make([]bool, len(lines))
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		switch {
		case fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			fence = trimmed[:3]
			inside[i] = true
		case fence != "" && strings.HasPrefix(trimmed, fence):
			inside[i] = true
			fence = ""
		default:
			inside[i] = fence != ""
		}
	}
	return inside
}

// inlineCodeSpan matches a backticked span. Its contents are sample syntax, so
// a "link" or a "path:line" inside one is not a claim either.
var inlineCodeSpan = regexp.MustCompile("`[^`\n]*`")

// stripInlineCode blanks backticked spans while preserving column positions, so
// a reported column still points at the right place in the source line.
func stripInlineCode(line string) string {
	return inlineCodeSpan.ReplaceAllStringFunc(line, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
}

// atxHeading matches a Markdown heading line and captures its text.
var atxHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)

// explicitAnchor matches a hand-written HTML anchor, e.g. <a id="foo"></a>.
var explicitAnchor = regexp.MustCompile(`<a\s+(?:id|name)\s*=\s*"([^"]+)"`)

// slugPunctuation is everything GitHub drops when it slugs a heading: it keeps
// letters, digits, underscores, hyphens and spaces and removes the rest.
var slugPunctuation = regexp.MustCompile(`[^\p{L}\p{N}_\- ]+`)

// headingSlug reproduces GitHub's heading anchor: strip Markdown emphasis and
// link syntax, lowercase, drop punctuation, then hyphenate spaces.
func headingSlug(text string) string {
	text = strings.ReplaceAll(text, "`", "")
	// A heading may itself be a link: use the link TEXT, which is what renders.
	text = markdownLink.ReplaceAllString(text, "$1")
	text = strings.NewReplacer("*", "", "_", "_", "~", "").Replace(text)
	text = strings.ToLower(strings.TrimSpace(text))
	text = slugPunctuation.ReplaceAllString(text, "")
	return strings.ReplaceAll(strings.TrimSpace(text), " ", "-")
}

// pageAnchors returns every anchor a reader can jump to on one page: the slug
// of each heading (with GitHub's -1, -2 … suffixes for repeats) and every
// explicit HTML anchor.
func pageAnchors(lines []string) map[string]bool {
	anchors := map[string]bool{}
	counts := map[string]int{}
	fenced := fencedLines(lines)

	for i, line := range lines {
		// A hand-written HTML anchor is matched by the browser EXACTLY as written,
		// so it is stored verbatim; heading slugs are lowercase by construction.
		for _, m := range explicitAnchor.FindAllStringSubmatch(line, -1) {
			anchors[m[1]] = true
		}
		if fenced[i] {
			continue
		}
		m := atxHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		slug := headingSlug(m[1])
		if slug == "" {
			continue
		}
		if n := counts[slug]; n > 0 {
			anchors[fmt.Sprintf("%s-%d", slug, n)] = true
		} else {
			anchors[slug] = true
		}
		counts[slug]++
	}
	return anchors
}

// markdownLink matches an inline Markdown link and captures text and target.
var markdownLink = regexp.MustCompile(`\[([^\]\[]*)\]\(([^)\s]*)(?:\s+"[^"]*")?\)`)

// docLink is one link found in the governed corpus, kept with enough position
// to print a clickable file:line for whoever has to fix it.
type docLink struct {
	file   string
	line   int
	target string
}

// maskNonProse returns the page with fenced blocks and inline code spans blanked
// out, preserving every byte offset and line break. Scanning the masked DOCUMENT
// rather than each line is what catches a link whose text wraps across lines —
// "[ARCHITECTURE.md §15 — Error\nClassification](…)" is one link, and a
// line-scoped scanner silently sees none.
func maskNonProse(lines []string) string {
	fenced := fencedLines(lines)
	masked := make([]string, len(lines))
	for i, line := range lines {
		if fenced[i] {
			masked[i] = strings.Repeat(" ", len(line))
			continue
		}
		masked[i] = stripInlineCode(line)
	}
	return strings.Join(masked, "\n")
}

// governedLinks collects every inline Markdown link outside fenced blocks and
// inline code spans across the corpus.
func governedLinks(t testing.TB, root string, files []string) []docLink {
	t.Helper()
	var links []docLink
	for _, rel := range files {
		masked := maskNonProse(readGoverned(t, root, rel))
		for _, loc := range markdownLink.FindAllStringSubmatchIndex(masked, -1) {
			links = append(links, docLink{
				file:   rel,
				line:   strings.Count(masked[:loc[0]], "\n") + 1,
				target: masked[loc[4]:loc[5]],
			})
		}
	}
	require.NotEmpty(t, links, "no Markdown links parsed from the governed corpus — the scanner is broken, not the docs")
	return links
}

// walkTreeFiles is used by the length check to reach files markdownFiles skips.
func walkTreeFiles(t testing.TB, dir string, suffix string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), suffix) {
			out = append(out, path)
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(out)
	return out
}
