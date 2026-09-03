package docsexamples_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The governance checks are only worth their runtime if the scanner they share
// sees what a reader sees. A silent false NEGATIVE is the dangerous failure:
// every page passes, the report is green, and the broken links are still there.
// These cases pin the four places the scanner could go quietly blind — a link
// whose text wraps, a heading slug, a fenced block, and a code span.
//
// Category: unit (TESTS.md §1).

func TestMarkdownScanner_FindsALinkWhoseTextWraps(t *testing.T) {
	// A wrapped link is ordinary Markdown and renders as one link. A line-scoped
	// scanner sees neither half, so every anchor written this way goes unchecked.
	doc := maskNonProse([]string{
		"see [ARCHITECTURE.md §15 — Error",
		"Classification](../ARCHITECTURE.md#15-error-classification) for the model",
	})
	matches := markdownLink.FindAllStringSubmatch(doc, -1)
	require.Len(t, matches, 1, "a link whose text wraps across lines must be found once")
	require.Equal(t, "../ARCHITECTURE.md#15-error-classification", matches[0][2])
}

func TestMarkdownScanner_IgnoresFencedAndInlineCode(t *testing.T) {
	doc := maskNonProse([]string{
		"```yaml",
		"see [not a link](nowhere.md)",
		"```",
		"prose `[also not](gone.md)` and [real](docs/index.md)",
	})
	var targets []string
	for _, m := range markdownLink.FindAllStringSubmatch(doc, -1) {
		targets = append(targets, m[2])
	}
	require.Equal(t, []string{"docs/index.md"}, targets,
		"only the prose link is a claim; the fenced and backticked ones are sample syntax")
}

func TestMarkdownScanner_SlugsHeadingsAsGitHubDoes(t *testing.T) {
	for _, c := range []struct{ heading, want string }{
		{"## Setup 5 — Confirm window (auto-revert on failure)", "setup-5--confirm-window-auto-revert-on-failure"},
		{"### `routes[].policy.backoff` -- Retry Backoff", "routespolicybackoff----retry-backoff"},
		{"## 15. Error Classification", "15-error-classification"},
		{"### Ingress byte model", "ingress-byte-model"},
		{"## Adapter & runtime diagnostic metrics", "adapter--runtime-diagnostic-metrics"},
		{"### MQTT (`adapters/mqtt/transport/paho`)", "mqtt-adaptersmqtttransportpaho"},
	} {
		text := strings.TrimLeft(c.heading, "# ")
		require.Equal(t, c.want, headingSlug(text), "slug for %q", c.heading)
	}
}

func TestMarkdownScanner_NumbersRepeatedHeadings(t *testing.T) {
	anchors := pageAnchors([]string{"## Notes", "## Notes", "## Notes"})
	require.True(t, anchors["notes"] && anchors["notes-1"] && anchors["notes-2"],
		"GitHub disambiguates repeated headings with -1, -2; a link to the second one must resolve")
}
