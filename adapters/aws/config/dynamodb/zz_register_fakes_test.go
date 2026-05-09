package dynamodb_test

import "github.com/mariotoffia/gobridge/ports"

// stubPluginConfig is a permissive PluginConfig stand-in registered for
// transport kinds that the loader_test fixtures reference (e.g. "mqtt")
// but whose real adapter packages are not imported into this test
// binary. Without these registrations config.Parse rejects the JSON
// round-trip with "unknown plugin kind".
type stubPluginConfig struct{ kind string }

func (s stubPluginConfig) Kind() string    { return s.kind }
func (s stubPluginConfig) Validate() error { return nil }

func init() {
	for _, k := range []string{"mqtt", "sqs", "http"} {
		kind := k
		func() {
			defer func() { _ = recover() }()
			ports.DefaultRegistry.Register(kind, func(_ ports.RawConfig) (ports.PluginConfig, error) {
				return stubPluginConfig{kind: kind}, nil
			})
		}()
	}
}
