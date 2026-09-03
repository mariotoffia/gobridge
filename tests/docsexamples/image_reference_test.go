package docsexamples_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The published image is ghcr.io/mariotoffia/gobridge. Every stable
// cmd/gobridge/vX.Y.Z release pushes it BY DIGEST and records that digest in
// the gobridge-image-digest.txt asset of the release; the one mutable tag,
// latest, is promoted afterwards from the same scanned digest and only when
// the release is the highest stable one (RELEASE.md, "Image publication").
// Two things follow for the documentation:
//
//   - a task definition, pod spec, CDK construct or Dockerfile that names the
//     image must pin it by digest — a tag-form reference in a spec fence is a
//     deployment that can change under the operator;
//   - no page may say the image is not published yet, or that no tag exists.
//
// Both are checked here so the text cannot drift from the registry again.
//
// Category: unit (TESTS.md §1).

const publishedImage = "ghcr.io/mariotoffia/gobridge"

// imageTextFiles is the corpus: the governed Markdown (docs/** and README.md)
// plus the root release and development guides and every deployment README,
// which are where image references live.
func imageTextFiles(t *testing.T, root string) []string {
	t.Helper()
	files := markdownFiles(t, root)
	for _, extra := range []string{"RELEASE.md", "DEVELOPMENT.md"} {
		if _, err := os.Stat(filepath.Join(root, extra)); err == nil {
			files = append(files, extra)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "deployment"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(files)
	return files
}

// tagFormRE matches the image followed by a tag separator. The digest form
// (`@sha256:`) and the bare name do not match.
var tagFormRE = regexp.MustCompile(regexp.QuoteMeta(publishedImage) + `:`)

// TestPublishedImageReferences_FencesPinByDigest fails on any fenced line
// that references the published image by tag. The single allowance is the
// command that RESOLVES a tag to its digest (`docker buildx imagetools
// inspect ...:latest`): that line exists precisely so the reader can pin.
func TestPublishedImageReferences_FencesPinByDigest(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, rel := range imageTextFiles(t, root) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		lines := strings.Split(string(data), "\n")
		fenced := fencedLines(lines)
		for i, line := range lines {
			if !fenced[i] || !tagFormRE.MatchString(line) {
				continue
			}
			if strings.Contains(line, "imagetools inspect") {
				continue
			}
			offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	require.Empty(t, offenders,
		"fenced references to %s must pin by digest (`%s@sha256:...`); a tag can move under a deployment.\n%s",
		publishedImage, publishedImage, strings.Join(offenders, "\n"))
}

// staleClaims are the sentences that were true before the first command
// release and are false now. Each one has appeared in a published page.
var staleClaims = []string{
	"will be published",
	"No image tags are published yet",
	"at the first command release",
	"until the first release",
	"once the first `cmd/gobridge",
	"after the first `cmd/gobridge",
}

// TestPublishedImageText_DoesNotDenyTheRegistry fails on any page that still
// describes the image as unpublished or tagless.
func TestPublishedImageText_DoesNotDenyTheRegistry(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, rel := range imageTextFiles(t, root) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		// A sentence wraps; match on the document with newlines turned into
		// spaces (same length, so an offset maps straight back to a line).
		text := string(data)
		flat := strings.ReplaceAll(text, "\n", " ")
		for _, claim := range staleClaims {
			for at := strings.Index(flat, claim); at >= 0; {
				line := strings.Count(text[:at], "\n") + 1
				offenders = append(offenders, rel+":"+strconv.Itoa(line)+": "+claim)
				next := strings.Index(flat[at+len(claim):], claim)
				if next < 0 {
					break
				}
				at += len(claim) + next
			}
		}
	}
	require.Empty(t, offenders,
		"the image IS published (by digest, with a guarded `latest`); rewrite these lines.\n%s",
		strings.Join(offenders, "\n"))
}
