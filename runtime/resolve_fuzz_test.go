package runtime

// ═══════════════════════════════════════════════
// Fuzz Tests for RenderAddress
//
// FuzzValidateMQTTTopic was moved to
// adapters/mqtt/transport/paho/topic_validator_test.go as part of
// MQTT topic validation lives next to the paho factory now.
// ═══════════════════════════════════════════════

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/runtime/route"
)

// FuzzRenderAddress verifies that arbitrary template+header combinations never
// cause a panic or a hang.
//
// A hang is left to the fuzzing engine, which is what detects a worker that
// stops responding and saves the input that did it. An earlier version raced
// each call against a 100 ms wall clock in a goroutine, which is not a test of
// RenderAddress at all: fuzzing saturates every core by design, so the budget
// measured how busy the machine was. It duly failed on a host whose antivirus
// was mid-scan and wrote the "crasher" below into the corpus, where it would
// have stayed forever — the same input renders in 4 microseconds. Its shape is
// kept as a seed, which is the part that was worth keeping.
func FuzzRenderAddress(f *testing.F) {
	f.Add("devices/{id}/cmd", "id", "device-42")
	f.Add("{x}", "x", "{x}")
	f.Add("{a}/{b}", "a", "val-{b}")
	f.Add("", "k", "v")
	f.Add("{}", "k", "v")
	f.Add("{{nested}}", "nested", "inner")
	f.Add("{k}", "k", "")
	f.Add("no-placeholders", "k", "v")
	f.Add("{missing}", "other", "v")
	f.Add("prefix{key}suffix", "key", "{key}")
	// A long run of control bytes ending in an unclosed brace, with a
	// non-UTF-8 substitution value: the parser edge a mutation run found.
	f.Add(strings.Repeat("\x17", 130)+"{", "0", "\x80")

	f.Fuzz(func(_ *testing.T, template, headerKey, headerVal string) {
		_, _ = route.RenderAddress(template, map[string]any{headerKey: headerVal})
	})
}
