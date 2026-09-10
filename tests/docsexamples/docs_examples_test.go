// Package docsexamples_test guards the documentation against
// config-shape drift: every COMPLETE bridge-config YAML example in
// docs/**/*.md (and the root README.md) must strict-decode through the
// real two-stage config parser (config/parser.Parse) with a plugin
// registry containing every in-repo transport and store decoder — the
// same composition a production binary performs (see cmd/gobridge and
// deployment/aws-filebased-config/lib/bootstrap).
//
// Classification: a fenced ```yaml block is a COMPLETE blueprint
// (UBIQUITOUS.md — "BridgeConfig", the aggregate root of the
// blueprint) iff its top-level mapping carries the `bridge` key —
// the only non-omitempty top-level field of the parser's stage-1
// document shape (config/parser/parse.go, stage1Bridge). Blocks whose
// YAML is malformed but that textually contain a column-0 `bridge:`
// key are still classified complete so broken examples FAIL loudly
// instead of being silently skipped. Everything else (single-section
// fragments, key tables, snippets) is skipped.
//
// Escape hatch: a docs author can exempt an intentionally
// illustrative or deliberately invalid complete example by placing
//
//	<!-- docs-example: skip -->
//
// on its own line directly above the opening ```yaml fence (blank
// lines between the marker and the fence are tolerated).
//
// Category: unit (TESTS.md §1) — no Docker, no network, no goroutines,
// no sleeps; the markdown files under docs/ are the fixtures.
package docsexamples_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
)

// skipMarkerRE matches the escape-hatch HTML comment documented in the
// package comment.
var skipMarkerRE = regexp.MustCompile(`<!--\s*docs-example:\s*skip\s*-->`)

// headingRE matches an ATX markdown heading (# .. ######).
var headingRE = regexp.MustCompile(`^#{1,6}\s+(.*)$`)

// topLevelBridgeKeyRE is the textual fallback discriminator used when a
// block does not even parse as YAML: a column-0 `bridge:` key marks it
// as a complete blueprint so the parse error surfaces as a failure.
var topLevelBridgeKeyRE = regexp.MustCompile(`(?m)^bridge\s*:`)

// yamlBlock is one fenced ```yaml block extracted from a markdown file.
type yamlBlock struct {
	heading string // nearest preceding markdown heading, "" when none
	line    int    // 1-based line number of the opening fence
	body    string // fence content, fence indent stripped
	skip    bool   // an escape-hatch marker sits directly above the fence
}

// extractYAMLBlocks scans markdown source line by line and returns
// every fenced ```yaml block in document order. Fences indented inside
// lists are supported: the opening fence's indentation is stripped
// from each body line. Deterministic and pure — no I/O.
func extractYAMLBlocks(src string) []yamlBlock {
	lines := strings.Split(src, "\n")

	var (
		out     []yamlBlock
		heading string
		inFence bool
		current yamlBlock
		indent  string
		body    []string
	)

	for i, raw := range lines {
		if inFence {
			trimmed := strings.TrimSpace(raw)
			if strings.HasPrefix(trimmed, "```") {
				current.body = strings.Join(body, "\n")
				out = append(out, current)
				inFence = false
				continue
			}
			body = append(body, strings.TrimPrefix(raw, indent))
			continue
		}

		if m := headingRE.FindStringSubmatch(raw); m != nil {
			heading = strings.TrimSpace(m[1])
			continue
		}

		trimmed := strings.TrimLeft(raw, " \t")
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		info := strings.TrimPrefix(trimmed, "```")
		lang, _, _ := strings.Cut(strings.TrimSpace(info), " ")
		if lang != "yaml" && lang != "yml" {
			continue
		}

		inFence = true
		indent = raw[:len(raw)-len(trimmed)]
		body = body[:0]
		current = yamlBlock{
			heading: heading,
			line:    i + 1,
			skip:    hasSkipMarkerAbove(lines, i),
		}
	}

	return out
}

// hasSkipMarkerAbove reports whether the first non-blank line above
// lines[fenceIdx] is the escape-hatch marker.
func hasSkipMarkerAbove(lines []string, fenceIdx int) bool {
	for j := fenceIdx - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" {
			continue
		}
		return skipMarkerRE.MatchString(trimmed)
	}
	return false
}

// isCompleteBridgeConfig implements the classification rule from the
// package comment: top-level mapping with a `bridge` key, with a
// textual fallback for malformed YAML so broken complete examples fail
// the decode subtest rather than being skipped as fragments.
func isCompleteBridgeConfig(body string) bool {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return topLevelBridgeKeyRE.MatchString(body)
	}
	_, ok := doc["bridge"]
	return ok
}

// newFullRegistry composes the plugin-config registry the way a
// production composition root does (cmd/gobridge/main.go,
// deployment/aws-filebased-config/lib/bootstrap/config.go), extended
// with every remaining in-repo Register so docs may reference any
// shipped transport or store kind: mqtt/mqtt.paho, sqs/aws.sqs,
// servicebus (+ fully-qualified form), amqp091, amqp10, http, memory,
// sqlite, dynamodb.
func newFullRegistry(t *testing.T) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	require.NoError(t, errors.Join(
		amqp091.Register(reg),
		amqp10.Register(reg),
		awsstore.Register(reg),
		sqs.Register(reg),
		servicebus.Register(reg),
		httptransport.Register(reg),
		paho.Register(reg),
		nativestore.Register(reg),
	), "composing the full plugin registry")
	return reg
}

// repoRoot resolves the repository root relative to this module
// (tests/docsexamples) and sanity-checks the docs tree exists.
func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(root, "docs"),
		"docs/ not found — harness must live at <repo>/tests/docsexamples")
	return root
}

// markdownFiles returns the repo-relative, sorted list of markdown
// files to scan: docs/**/*.md plus the root README.md when present.
func markdownFiles(t testing.TB, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking docs tree at %s: %w", path, err)
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	if _, statErr := os.Stat(filepath.Join(root, "README.md")); statErr == nil {
		files = append(files, "README.md")
	}
	sort.Strings(files)
	return files
}

// TestDocsExamples_CompleteConfigsStrictDecode is the CI guard: every
// complete bridge-config YAML example in the documentation must
// strict-decode through config/parser.Parse with the full in-repo
// plugin registry. Failure output names the file, nearest heading,
// fence line, and the decode error.
func TestDocsExamples_CompleteConfigsStrictDecode(t *testing.T) {
	root := repoRoot(t)
	reg := newFullRegistry(t)

	var complete int
	for _, rel := range markdownFiles(t, root) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)

		for _, b := range extractYAMLBlocks(string(data)) {
			if b.skip || !isCompleteBridgeConfig(b.body) {
				continue
			}
			complete++
			t.Run(fmt.Sprintf("%s:%d", rel, b.line), func(t *testing.T) {
				_, decodeErr := cfgparser.Parse(strings.NewReader(b.body), cfgparser.FormatYAML, reg)
				if decodeErr != nil {
					t.Fatalf("complete bridge-config example failed strict decode\n"+
						"  file:    %s\n"+
						"  heading: %s\n"+
						"  fence:   line %d\n"+
						"  error:   %v\n\n"+
						"fix the example, or — if it is intentionally illustrative — put\n"+
						"<!-- docs-example: skip --> on the line above the ```yaml fence",
						rel, b.heading, b.line, decodeErr)
				}
			})
		}
	}

	require.Positive(t, complete,
		"no complete bridge-config examples found — extraction or classification is broken")
}

// TestExtractYAMLBlocks_ClassificationAndSkip protects the harness
// itself: extraction, the complete-vs-fragment discriminator, the
// malformed-YAML fallback, indent stripping, and the skip marker.
func TestExtractYAMLBlocks_ClassificationAndSkip(t *testing.T) {
	tests := []struct {
		name         string
		markdown     string
		wantBlocks   int
		wantComplete []bool
		wantSkip     []bool
		wantHeading  []string
		wantLine     []int
	}{
		{
			name:         "complete config under heading",
			markdown:     "## Full example\n\n```yaml\nbridge:\n  id: b1\n```\n",
			wantBlocks:   1,
			wantComplete: []bool{true},
			wantSkip:     []bool{false},
			wantHeading:  []string{"Full example"},
			wantLine:     []int{3},
		},
		{
			name:         "fragment without bridge key",
			markdown:     "```yaml\nreceivers:\n  - id: r1\n    transport: mqtt\n```\n",
			wantBlocks:   1,
			wantComplete: []bool{false},
			wantSkip:     []bool{false},
			wantHeading:  []string{""},
			wantLine:     []int{1},
		},
		{
			name:         "skip marker directly above with blank line",
			markdown:     "<!-- docs-example: skip -->\n\n```yaml\nbridge:\n  id: b1\n```\n",
			wantBlocks:   1,
			wantComplete: []bool{true},
			wantSkip:     []bool{true},
			wantHeading:  []string{""},
			wantLine:     []int{3},
		},
		{
			name:         "malformed yaml with bridge key still classified complete",
			markdown:     "```yaml\nbridge:\n  id: [unclosed\n```\n",
			wantBlocks:   1,
			wantComplete: []bool{true},
			wantSkip:     []bool{false},
			wantHeading:  []string{""},
			wantLine:     []int{1},
		},
		{
			name:       "non-yaml fence ignored",
			markdown:   "```bash\necho bridge:\n```\n\n```json\n{\"bridge\": {}}\n```\n",
			wantBlocks: 0,
		},
		{
			name:         "indented fence in list has indent stripped",
			markdown:     "1. Create it:\n\n   ```yaml\n   bridge:\n     id: b1\n   ```\n",
			wantBlocks:   1,
			wantComplete: []bool{true},
			wantSkip:     []bool{false},
			wantHeading:  []string{""},
			wantLine:     []int{3},
		},
		{
			name:         "two blocks track nearest heading",
			markdown:     "# A\n\n```yaml\nbridge: {id: b1}\n```\n\n## B\n\n```yaml\nroutes: []\n```\n",
			wantBlocks:   2,
			wantComplete: []bool{true, false},
			wantSkip:     []bool{false, false},
			wantHeading:  []string{"A", "B"},
			wantLine:     []int{3, 9},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks := extractYAMLBlocks(tc.markdown)
			require.Len(t, blocks, tc.wantBlocks)
			for i, b := range blocks {
				require.Equalf(t, tc.wantComplete[i], isCompleteBridgeConfig(b.body), "block %d complete", i)
				require.Equalf(t, tc.wantSkip[i], b.skip, "block %d skip", i)
				require.Equalf(t, tc.wantHeading[i], b.heading, "block %d heading", i)
				require.Equalf(t, tc.wantLine[i], b.line, "block %d line", i)
			}
		})
	}
}
