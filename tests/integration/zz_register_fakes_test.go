package integration_test

import (
	"errors"

	"github.com/mariotoffia/gobridge/ports"
)

// fakePluginConfig is a permissive stand-in for adapter Configs whose
// real decoders are not imported into the integration_test binary.
// Tests construct YAML blueprints with transports like "exclusive" or
// "broken" and store types like "memory"; without registered decoders,
// ParseFile would reject those blueprints before the supervisor's own
// transport/store-factory map is consulted.
type fakePluginConfig struct {
	kind string
}

func (f fakePluginConfig) Kind() string  { return f.kind }
func (fakePluginConfig) Validate() error { return nil }

// fakeOverlayConfig is a typed PluginConfig used by the DDB overlay /
// supervisor tests. It carries a single Broker field so we can assert
// that overlays correctly merge nested transport config and that the
// JSON round-trip through DDB preserves the typed payload.
type fakeOverlayConfig struct {
	Broker string `yaml:"broker" json:"broker" mapstructure:"broker"`
}

func (fakeOverlayConfig) Kind() string    { return "fake" }
func (fakeOverlayConfig) Validate() error { return nil }

// registerFakes installs stand-in PluginConfig decoders for the
// transport / store kinds the integration test fixtures reference.
// It is invoked once per test registry by the helpers that build
// hermetic registries; duplicate calls return ports.ErrDuplicateKind
// and are ignored on the second register, which keeps the helper
// idempotent across tests that share a process-wide test fixture
// registry.
func registerFakes(reg *ports.Registry) error {
	registerSimple := func(kind string) error {
		k := kind
		return reg.Register(k, func(_ ports.RawConfig) (ports.PluginConfig, error) {
			return fakePluginConfig{kind: k}, nil
		})
	}

	if err := errors.Join(
		// kind "fake" decodes into fakeOverlayConfig so the broker field
		// survives a wire-format round-trip through ParseFile.
		reg.Register("fake", func(raw ports.RawConfig) (ports.PluginConfig, error) {
			var c fakeOverlayConfig
			if raw != nil {
				if err := raw.Decode(&c); err != nil {
					return nil, err
				}
			}
			return c, nil
		}),
		// Other transport discriminators used by test fixtures.
		registerSimple("exclusive"),
		registerSimple("broken"),
		// Store discriminators used by test fixtures.
		registerSimple("memory"),
	); err != nil {
		return err
	}
	return nil
}

// newTestRegistry returns a fresh *ports.Registry pre-populated with
// the integration-test fakes. Use this everywhere a parser is called
// in this test binary instead of constructing a bare NewRegistry().
func newTestRegistry() *ports.Registry {
	reg := ports.NewRegistry()
	if err := registerFakes(reg); err != nil {
		panic("integration_test: register fakes: " + err.Error())
	}
	return reg
}
