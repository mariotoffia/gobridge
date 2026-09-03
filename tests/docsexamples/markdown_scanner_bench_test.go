package docsexamples_test

import (
	"fmt"
	"strings"
	"testing"
)

// These gates run on every pull request, so what they cost is a property worth
// keeping honest: a scanner that grows superlinearly in the corpus turns a
// documentation edit into a slow CI step, and the usual reaction to a slow gate
// is to stop running it.
//
// The simple cases measure one page's worth of work. The corpus cases measure
// the real gate. The synthetic case multiplies the real corpus so the scaling —
// not just the absolute number — is visible.

// benchCorpus loads the governed corpus once into memory so the benchmarks
// measure scanning, not disk.
func benchCorpus(b *testing.B) (root string, files []string, pages [][]string) {
	b.Helper()
	root = repoRoot(b)
	files = governedMarkdown(b, root)
	pages = make([][]string, len(files))
	for i, rel := range files {
		pages[i] = readGoverned(b, root, rel)
	}
	return root, files, pages
}

func BenchmarkHeadingSlug(b *testing.B) {
	const heading = "Setup 5 — Confirm window (auto-revert on failure)"
	b.ReportAllocs()
	for b.Loop() {
		_ = headingSlug(heading)
	}
}

func BenchmarkPageAnchors(b *testing.B) {
	_, files, pages := benchCorpus(b)
	// The largest governed page is the worst case a single lookup pays.
	widest := 0
	for i := range pages {
		if len(pages[i]) > len(pages[widest]) {
			widest = i
		}
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(strings.Join(pages[widest], "\n"))))
	b.Logf("widest governed page: %s (%d lines)", files[widest], len(pages[widest]))
	for b.Loop() {
		_ = pageAnchors(pages[widest])
	}
}

func BenchmarkMaskNonProse(b *testing.B) {
	_, _, pages := benchCorpus(b)
	b.ReportAllocs()
	for b.Loop() {
		for _, lines := range pages {
			_ = maskNonProse(lines)
		}
	}
}

// BenchmarkCorpusLinkScan measures the whole gate: every governed page parsed
// for links, then every page's anchors built to resolve the fragments.
func BenchmarkCorpusLinkScan(b *testing.B) {
	root, files, pages := benchCorpus(b)
	b.ReportAllocs()
	for b.Loop() {
		links := governedLinks(b, root, files)
		anchors := make(map[string]map[string]bool, len(files))
		for i, rel := range files {
			anchors[rel] = pageAnchors(pages[i])
		}
		if len(links) == 0 || len(anchors) == 0 {
			b.Fatal("scan produced nothing")
		}
	}
}

// BenchmarkSyntheticCorpusScan multiplies the real corpus so the scan's scaling
// is measurable rather than inferred from one data point.
func BenchmarkSyntheticCorpusScan(b *testing.B) {
	_, _, pages := benchCorpus(b)
	for _, multiple := range []int{1, 4, 16} {
		grown := make([][]string, 0, len(pages)*multiple)
		for range multiple {
			grown = append(grown, pages...)
		}
		b.Run(fmt.Sprintf("x%d", multiple), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, lines := range grown {
					masked := maskNonProse(lines)
					_ = markdownLink.FindAllStringSubmatchIndex(masked, -1)
					_ = pageAnchors(lines)
				}
			}
		})
	}
}
