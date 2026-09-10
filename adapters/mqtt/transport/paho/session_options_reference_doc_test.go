package paho_test

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
)

// The session options table is the only place an operator can discover what
// `options.session.*` accepts. A key the decoder reads but the table omits is a
// guard nobody can find — `assert_stable_client_identity` is precisely that: the
// factory REFUSES a persistent session with a hostname suffix and names the flag
// in the refusal, so an operator meets the name for the first time in an error
// message with no page to look it up on. A key the table invents is worse: it
// will be configured, silently ignored by the strict decoder's sibling paths,
// and never take effect.
//
// Both directions are derived from the struct the decoder populates, so the page
// cannot drift from it without failing this package's build gate.
//
// Category: unit (TESTS.md §1) — the page is the fixture.

const sessionOptionsDoc = "../../../../docs/transports/mqtt-options.md"

const sessionOptionsHeading = "## Session Options Reference"

// optionRow matches a table row whose first cell is a backticked option key,
// including a nested one written in dotted form ("will.topic", "tls.enable").
var optionRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_.]+)`\\s*\\|")

// documentedOptions collects the option names of the first table under heading,
// stopping at the next heading of the same or higher level. Fenced blocks are
// skipped so a YAML example containing a "#" line cannot truncate the scan.
func documentedOptions(t *testing.T, page, heading string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(page)
	require.NoError(t, err, "the options page must exist")

	level := strings.Count(heading, "#")
	options := map[string]bool{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, heading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if depth := len(line) - len(strings.TrimLeft(line, "#")); depth > 0 && depth <= level {
			break
		}
		if match := optionRow.FindStringSubmatch(line); match != nil {
			options[match[1]] = true
		}
	}
	require.NotEmptyf(t, options, "no option rows parsed under %q in %s — the table shape changed",
		heading, page)
	return options
}

// decodedKeys returns the LEAF config keys the strict decoder reads for one
// options struct, in the dotted form the page publishes them: a nested struct
// contributes "will.topic", never a bare "will", because that is the key an
// operator actually writes in YAML.
func decodedKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	collectKeys(reflect.TypeOf(v), "", keys)
	require.NotEmpty(t, keys, "no mapstructure keys found on %T", v)
	return keys
}

func collectKeys(typ reflect.Type, prefix string, into map[string]bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("mapstructure"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		field := typ.Field(i).Type
		for field.Kind() == reflect.Pointer {
			field = field.Elem()
		}
		if field.Kind() == reflect.Struct && field.PkgPath() == typ.PkgPath() {
			collectKeys(field, path+".", into)
			continue
		}
		into[path] = true
	}
}

func missingFrom(want, have map[string]bool) []string {
	var out []string
	for name := range want {
		if !have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestSessionOptionsReference_DocumentsEveryDecodedKey(t *testing.T) {
	documented := documentedOptions(t, sessionOptionsDoc, sessionOptionsHeading)
	decoded := decodedKeys(t, paho.SessionOptions{})

	require.Emptyf(t, missingFrom(decoded, documented),
		"session keys the decoder reads that %s does not document; an operator meets them for the first time in an error message",
		sessionOptionsDoc)
	require.Emptyf(t, missingFrom(documented, decoded),
		"keys documented under %q that the decoder does not read; setting one would be silently ignored",
		sessionOptionsHeading)
}

func TestSenderOptionsReference_DocumentsEveryDecodedKey(t *testing.T) {
	const heading = "## Sender Options Reference"
	documented := documentedOptions(t, sessionOptionsDoc, heading)
	decoded := decodedKeys(t, paho.SenderOptions{})

	require.Emptyf(t, missingFrom(decoded, documented),
		"sender keys the decoder reads that %s does not document", sessionOptionsDoc)
	require.Emptyf(t, missingFrom(documented, decoded),
		"keys documented under %q that the decoder does not read", heading)
}
