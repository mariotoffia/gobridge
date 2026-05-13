// Package dynamodb_test holds the test-only stub PluginConfig used
// by loader_test for transport kinds whose real adapter packages
// are not imported into this test binary.
package dynamodb_test

// stubPluginConfig is a permissive PluginConfig stand-in registered for
// transport kinds that the loader_test fixtures reference (e.g. "mqtt")
// but whose real adapter packages are not imported into this test
// binary. Without these registrations config.Parse rejects the JSON
// round-trip with "unknown plugin kind".
type stubPluginConfig struct{ kind string }

func (s stubPluginConfig) Kind() string    { return s.kind }
func (s stubPluginConfig) Validate() error { return nil }
