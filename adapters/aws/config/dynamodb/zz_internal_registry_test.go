package dynamodb

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// internalStubPluginConfig is a permissive PluginConfig used by the
// internal (white-box) streams_internal_test.go to register decoders
// for kinds that appear in fixture JSON. Mirrors the dynamodb_test
// helper but lives inside package dynamodb.
type internalStubPluginConfig struct{ kind string }

func (s internalStubPluginConfig) Kind() string    { return s.kind }
func (s internalStubPluginConfig) Validate() error { return nil }

// newDDBTestRegistry returns a *ports.Registry pre-populated with
// permissive decoders for the kinds referenced by internal stream
// tests. The helper fails the test on any registration error so
// callers can pass the result directly into Loader.registry.
func newDDBTestRegistry(t testing.TB) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	kinds := []string{"mqtt", "sqs", "dynamodb", "http"}
	for _, k := range kinds {
		k := k
		if err := reg.Register(k, ports.ConfigDecoder(func(raw ports.RawConfig) (ports.PluginConfig, error) {
			return internalStubPluginConfig{kind: k}, nil
		})); err != nil {
			t.Fatalf("register %q decoder: %v", k, err)
		}
	}
	return reg
}
