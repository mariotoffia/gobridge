package ports_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// fakeConfig is a minimal PluginConfig used to assert decoder output
// is returned unchanged by Registry.Decode.
type fakeConfig struct {
	kind string
	val  string
}

func (f fakeConfig) Kind() string    { return f.kind }
func (f fakeConfig) Validate() error { return nil }

// fakeRaw is a stub RawConfig — Decode is unused in registry tests
// because registered decoders ignore raw and return a fixed value.
type fakeRaw struct{}

func (fakeRaw) Decode(any) error { return nil }

// Verifies Registry.Decode returns the registered decoder's output for a known kind.
func TestRegistry_Decode_ReturnsRegisteredDecoderOutput(t *testing.T) {
	r := ports.NewRegistry()
	want := fakeConfig{kind: "demo.kind", val: "hello"}

	r.Register("demo.kind", func(raw ports.RawConfig) (ports.PluginConfig, error) {
		return want, nil
	})

	got, err := r.Decode("demo.kind", fakeRaw{})
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Verifies Register panics when the same kind is registered twice.
func TestRegistry_Register_PanicsOnDuplicateKind(t *testing.T) {
	r := ports.NewRegistry()
	dec := func(ports.RawConfig) (ports.PluginConfig, error) { return fakeConfig{}, nil }

	r.Register("dup.kind", dec)

	require.PanicsWithValue(t, "ports: duplicate plugin kind dup.kind", func() {
		r.Register("dup.kind", dec)
	})
}

// Verifies Register panics on a nil decoder — registering nothing is a programming error.
func TestRegistry_Register_PanicsOnNilDecoder(t *testing.T) {
	r := ports.NewRegistry()
	require.PanicsWithValue(t, "ports: nil ConfigDecoder for kind x", func() {
		r.Register("x", nil)
	})
}

// Verifies Decode returns a clear "unknown plugin kind" error when no decoder is registered.
func TestRegistry_Decode_UnknownKind(t *testing.T) {
	r := ports.NewRegistry()

	got, err := r.Decode("missing.kind", fakeRaw{})
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "unknown plugin kind")
	assert.Contains(t, err.Error(), `"missing.kind"`)
}

// Verifies decoder errors are surfaced unchanged so callers can wrap them with their own context.
func TestRegistry_Decode_PropagatesDecoderError(t *testing.T) {
	r := ports.NewRegistry()
	sentinel := errors.New("decode boom")

	r.Register("err.kind", func(ports.RawConfig) (ports.PluginConfig, error) {
		return nil, sentinel
	})

	got, err := r.Decode("err.kind", fakeRaw{})
	assert.Nil(t, got)
	require.ErrorIs(t, err, sentinel)
}

// Verifies DefaultRegistry is non-nil and ready to use without further initialization.
func TestDefaultRegistry_NonNil(t *testing.T) {
	require.NotNil(t, ports.DefaultRegistry)
}

// Verifies concurrent Register and Decode calls are race-safe.
// Run with -race (the project's default) to surface any data races.
func TestRegistry_ConcurrentRegisterAndDecode(t *testing.T) {
	r := ports.NewRegistry()

	const writers = 16
	const readers = 16
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	// Writers register disjoint kinds to avoid duplicate-panic; the
	// goal is to exercise the mutex, not the dup-detection path.
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				kind := fmt.Sprintf("kind.%d.%d", w, i)
				r.Register(kind, func(ports.RawConfig) (ports.PluginConfig, error) {
					return fakeConfig{kind: kind}, nil
				})
			}
		}(w)
	}

	// Readers race against writers calling Decode; they accept either
	// a successful lookup or an "unknown plugin kind" miss — only a
	// data race or a panic should fail the test.
	for rIdx := 0; rIdx < readers; rIdx++ {
		go func(rIdx int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				kind := fmt.Sprintf("kind.%d.%d", rIdx%writers, i)
				_, err := r.Decode(kind, fakeRaw{})
				if err != nil && !strings.Contains(err.Error(), "unknown plugin kind") {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}(rIdx)
	}

	wg.Wait()
}
