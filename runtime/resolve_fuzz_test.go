package runtime

// ═══════════════════════════════════════════════
// Fuzz Tests for RenderAddress
//
// FuzzValidateMQTTTopic was moved to
// adapters/mqtt/transport/paho/topic_validator_test.go as part of
// AP-005 — MQTT topic validation lives next to the paho factory now.
// ═══════════════════════════════════════════════

import (
	"testing"
	"time"
)

// FuzzRenderAddress verifies that arbitrary template+header combinations
// never cause panics or infinite loops. The test has a per-input timeout
// built into the fuzz harness.
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

	f.Fuzz(func(t *testing.T, template, headerKey, headerVal string) {
		vars := map[string]any{headerKey: headerVal}

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = RenderAddress(template, vars)
		}()

		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("RenderAddress did not return within 100ms for template=%q key=%q val=%q", template, headerKey, headerVal)
		}
	})
}
